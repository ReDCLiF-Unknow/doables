// Package discordbot lets a Discord server use Doables lists as task boards.
//
// A channel becomes a board with /taskboard. Each task on it is a message (a
// "card") with Done and Assign buttons, and a thread under it for talking
// about it. Everything is stored in Doables, through its API, as the Discord
// user who did it: the bot makes each person a Doables identity the first time
// they use it, so "who added it" and "who finished it" mean someone, and the
// same tasks are on the web app, the CLI and phone notifications too.
package discordbot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"doables/internal/client"
	"doables/internal/store"
)

// Discord is the part of a Discord session the bot uses. FromSession provides
// it for a real one; tests use a fake.
type Discord interface {
	InteractionRespond(i *discordgo.Interaction, r *discordgo.InteractionResponse) error
	InteractionResponseEdit(i *discordgo.Interaction, e *discordgo.WebhookEdit) (*discordgo.Message, error)
	FollowupMessageCreate(i *discordgo.Interaction, p *discordgo.WebhookParams) (*discordgo.Message, error)
	ChannelMessageSendComplex(channelID string, m *discordgo.MessageSend) (*discordgo.Message, error)
	ChannelMessageEditComplex(m *discordgo.MessageEdit) (*discordgo.Message, error)
	MessageThreadStartComplex(channelID, messageID string, t *discordgo.ThreadStart) (*discordgo.Channel, error)
	// ChannelName is a channel's name, or "" if it cannot be found out.
	ChannelName(channelID string) string
}

// Config is what New needs.
type Config struct {
	Discord Discord
	// Doables is the server's API; its token is not used, as the bot always
	// acts as a particular person.
	Doables *client.Client
	// StatePath is the bot's own SQLite file. It holds everyone's Doables
	// token, so it must be kept as private as the Doables database.
	StatePath string
	// WebURL is the address people open Doables at, for links to it; empty
	// leaves the links out.
	WebURL string
	Log    *log.Logger
}

type Bot struct {
	dg     Discord
	api    *client.Client
	state  *state
	webURL string
	log    *log.Logger

	// boardLocks keeps changes to one board in order: a card is posted and
	// recorded before the next look at the list could post it a second time.
	boardLocks sync.Map // list ID -> *sync.Mutex
	// signupMu stops two quick clicks from making somebody twice.
	signupMu sync.Mutex

	// watching is the board owners whose live updates are being followed.
	watchMu  sync.Mutex
	watching map[string]bool
	runCtx   context.Context
}

func New(c Config) (*Bot, error) {
	st, err := openState(c.StatePath)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", c.StatePath, err)
	}
	lg := c.Log
	if lg == nil {
		lg = log.Default()
	}
	return &Bot{dg: c.Discord, api: c.Doables, state: st, webURL: strings.TrimRight(c.WebURL, "/"),
		log: lg, watching: map[string]bool{}}, nil
}

func (b *Bot) Close() error { return b.state.close() }

func (b *Bot) lockBoard(listID int64) func() {
	m, _ := b.boardLocks.LoadOrStore(listID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Commands are the slash commands, for registering with Discord.
func Commands() []*discordgo.ApplicationCommand {
	inGuilds := &[]discordgo.InteractionContextType{discordgo.InteractionContextGuild}
	manage := int64(discordgo.PermissionManageChannels)
	one := 1
	return []*discordgo.ApplicationCommand{
		{
			Name:        "taskboard",
			Description: "Make this channel a task board: its tasks are kept in Doables",
			Contexts:    inGuilds,
			// Only people who can manage channels see this one; server admins
			// can change that under Server Settings → Integrations.
			DefaultMemberPermissions: &manage,
			Options: []*discordgo.ApplicationCommandOption{{
				Type: discordgo.ApplicationCommandOptionString, Name: "name",
				Description: "What to call the list in Doables (default: this channel's name)", MaxLength: 80,
			}},
		},
		{
			Name: "task", Description: "Add a task to this channel's board", Contexts: inGuilds,
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "title", Description: "What needs doing",
					Required: true, MinLength: &one, MaxLength: 200},
				{Type: discordgo.ApplicationCommandOptionUser, Name: "for", Description: "Whose job it is (you can also choose later)"},
				{Type: discordgo.ApplicationCommandOptionString, Name: "details", Description: "Anything else they need to know", MaxLength: 500},
			},
		},
		{Name: "tasks", Description: "Show what is still to do on this channel's board", Contexts: inGuilds},
		{Name: "mine", Description: "Show the tasks that are yours, on every board", Contexts: inGuilds},
		{Name: "doables", Description: "Sign in to Doables in a browser, to see your tasks there too", Contexts: inGuilds},
	}
}

// problem is something to tell the person who asked, rather than a fault.
type problem string

func (p problem) Error() string { return string(p) }

const errBusy = problem("Doables is setting up a lot of new people right now. Please try again in a minute.")

// HandleInteraction answers a slash command or a button press.
func (b *Bot) HandleInteraction(ic *discordgo.InteractionCreate) {
	i := ic.Interaction
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		switch i.ApplicationCommandData().Name {
		case "taskboard":
			b.cmdTaskboard(ctx, i)
		case "task":
			b.cmdTask(ctx, i)
		case "tasks":
			b.cmdTasks(ctx, i)
		case "mine":
			b.cmdMine(ctx, i)
		case "doables":
			b.cmdDoables(ctx, i)
		}
	case discordgo.InteractionMessageComponent:
		action, arg, _ := strings.Cut(i.MessageComponentData().CustomID, ":")
		taskID, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			return
		}
		switch action {
		case "done":
			b.pressDone(ctx, i, taskID)
		case "assign":
			b.pressAssign(i, taskID)
		case "assignto", "unassign":
			b.chooseAssignee(ctx, i, taskID, action == "unassign")
		}
	}
}

// ---- answering --------------------------------------------------------

// say answers at once, visible only to the person who asked.
func (b *Bot) say(i *discordgo.Interaction, text string) {
	err := b.dg.InteractionRespond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: text, Flags: discordgo.MessageFlagsEphemeral,
			AllowedMentions: &discordgo.MessageAllowedMentions{}},
	})
	if err != nil {
		b.log.Printf("discord: answering: %v", err)
	}
}

// thinking says that an answer is coming, which Discord needs within three
// seconds; Doables can take longer than that to reach.
func (b *Bot) thinking(i *discordgo.Interaction, private bool) {
	data := &discordgo.InteractionResponseData{}
	if private {
		data.Flags = discordgo.MessageFlagsEphemeral
	}
	if err := b.dg.InteractionRespond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource, Data: data,
	}); err != nil {
		b.log.Printf("discord: answering: %v", err)
	}
}

// answer replaces the "thinking" message.
func (b *Bot) answer(i *discordgo.Interaction, text string) {
	none := []discordgo.MessageComponent{}
	if _, err := b.dg.InteractionResponseEdit(i, &discordgo.WebhookEdit{Content: &text, Components: &none,
		AllowedMentions: &discordgo.MessageAllowedMentions{}}); err != nil {
		b.log.Printf("discord: answering: %v", err)
	}
}

// failed tells the person what went wrong: what to do about it if it is a
// problem, and that it was not their fault otherwise.
func (b *Bot) failed(i *discordgo.Interaction, err error) {
	var p problem
	if errors.As(err, &p) {
		b.answer(i, string(p))
		return
	}
	b.log.Printf("discordbot: %v", err)
	b.answer(i, "Sorry, that didn't work: Doables could not be reached or said no. Please try again in a moment.")
}

// ---- people -----------------------------------------------------------

// caller is who pressed or typed something.
func caller(i *discordgo.Interaction) (id, name string) {
	if i.Member != nil {
		return i.Member.User.ID, displayName(i.Member, i.Member.User)
	}
	return i.User.ID, displayName(nil, i.User)
}

// displayName is what to call someone in Doables: their name on this server
// if they have one there, as that is how their colleagues know them.
func displayName(m *discordgo.Member, u *discordgo.User) string {
	name := ""
	switch {
	case m != nil && m.Nick != "":
		name = m.Nick
	case u.GlobalName != "":
		name = u.GlobalName
	default:
		name = u.Username
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Discord user"
	}
	return clip(name, 40)
}

// personFor returns who a Discord user is in Doables, making them somebody
// there the first time.
func (b *Bot) personFor(ctx context.Context, discordID, name string) (person, error) {
	b.signupMu.Lock()
	defer b.signupMu.Unlock()
	p, ok, err := b.state.person(discordID)
	if err != nil || ok {
		return p, err
	}
	u, token, err := b.api.Register(ctx, name)
	if client.IsStatus(err, http.StatusTooManyRequests) {
		return p, errBusy
	}
	if err != nil {
		return p, fmt.Errorf("registering %s: %w", discordID, err)
	}
	p = person{DiscordID: discordID, DoablesID: u.ID, Token: token}
	return p, b.state.savePerson(p)
}

// member returns who a Discord user is in Doables, having made sure they are
// on the board's list: only members can be given its tasks.
func (b *Bot) member(ctx context.Context, bd board, discordID, name string) (person, error) {
	p, err := b.personFor(ctx, discordID, name)
	if err != nil || b.state.joined(discordID, bd.ListID) {
		return p, err
	}
	_, err = b.api.As(p.Token).Join(ctx, bd.Invite)
	if client.IsStatus(err, http.StatusNotFound) {
		// The owner made a new invite link in the web app, which stops the
		// old one working. They can still see the new one.
		if bd, err = b.freshInvite(ctx, bd); err == nil {
			_, err = b.api.As(p.Token).Join(ctx, bd.Invite)
		}
	}
	if err != nil {
		return p, fmt.Errorf("joining list %d: %w", bd.ListID, err)
	}
	return p, b.state.markJoined(discordID, bd.ListID)
}

func (b *Bot) freshInvite(ctx context.Context, bd board) (board, error) {
	owner, ok, err := b.state.person(bd.Owner)
	if err != nil || !ok {
		return bd, fmt.Errorf("board %d has no owner: %v", bd.ListID, err)
	}
	ls, err := b.api.As(owner.Token).Lists(ctx)
	if err != nil {
		return bd, err
	}
	for _, l := range ls {
		if l.ID == bd.ListID && l.InviteCode != "" {
			bd.Invite = l.InviteCode
			return bd, b.state.saveBoard(bd)
		}
	}
	return bd, problem("This board's list is gone from Doables, or its owner has left it. Someone can run /taskboard again to start a new one.")
}

// boardHere is the board in the channel the interaction came from. When there
// is none it says so and returns false.
func (b *Bot) boardHere(i *discordgo.Interaction) (board, bool) {
	bd, ok, err := b.state.board(i.ChannelID)
	if err != nil {
		b.log.Printf("discordbot: %v", err)
		b.say(i, "Sorry, something went wrong. Please try again.")
		return bd, false
	}
	if !ok {
		b.say(i, "This channel isn't a task board. Someone who can manage channels can make it one with `/taskboard`.")
	}
	return bd, ok
}

// ---- commands ---------------------------------------------------------

func option(i *discordgo.Interaction, name string) *discordgo.ApplicationCommandInteractionDataOption {
	for _, o := range i.ApplicationCommandData().Options {
		if o.Name == name {
			return o
		}
	}
	return nil
}

func (b *Bot) cmdTaskboard(ctx context.Context, i *discordgo.Interaction) {
	if _, ok, err := b.state.board(i.ChannelID); err == nil && ok {
		b.say(i, "This channel is already a task board. Add a task with `/task`.")
		return
	}
	name := ""
	if o := option(i, "name"); o != nil {
		name = strings.TrimSpace(o.StringValue())
	}
	if name == "" {
		name = b.dg.ChannelName(i.ChannelID)
	}
	if name == "" {
		name = "Tasks"
	}
	b.thinking(i, false)

	id, who := caller(i)
	p, err := b.personFor(ctx, id, who)
	if err != nil {
		b.failed(i, err)
		return
	}
	l, err := b.api.As(p.Token).CreateList(ctx, clip(name, 80))
	if err != nil {
		b.failed(i, err)
		return
	}
	bd := board{ChannelID: i.ChannelID, GuildID: i.GuildID, ListID: l.ID, Owner: id, Invite: l.InviteCode}
	if err := b.state.saveBoard(bd); err != nil {
		b.failed(i, err)
		return
	}
	b.state.markJoined(id, l.ID) // the owner is on their own list
	b.watch(bd.Owner)

	text := fmt.Sprintf("📋 This channel is now a task board, kept in Doables as **%s**.\n"+
		"• `/task` adds a task. Each one gets **Done** and **Assign** buttons, and a thread for talking about it.\n"+
		"• `/tasks` shows what is left; `/mine` shows what is yours.", l.Name)
	if b.webURL != "" {
		text += fmt.Sprintf("\n• `/doables` signs you in on the web, at <%s>.", b.webURL)
	}
	b.answer(i, text)
}

func (b *Bot) cmdTask(ctx context.Context, i *discordgo.Interaction) {
	bd, ok := b.boardHere(i)
	if !ok {
		return
	}
	title := strings.TrimSpace(option(i, "title").StringValue())
	details := ""
	if o := option(i, "details"); o != nil {
		details = strings.TrimSpace(o.StringValue())
	}
	var forUser *discordgo.User
	var forMember *discordgo.Member
	if o := option(i, "for"); o != nil {
		uid, _ := o.Value.(string)
		if r := i.ApplicationCommandData().Resolved; r != nil {
			forUser, forMember = r.Users[uid], r.Members[uid]
		}
		if forUser == nil {
			b.say(i, "I couldn't find that person. Try choosing them again.")
			return
		}
		if forUser.Bot {
			b.say(i, "Bots can't do tasks. Choose a person, or leave it out and press **Assign** later.")
			return
		}
	}
	b.thinking(i, true)

	unlock := b.lockBoard(bd.ListID)
	defer unlock()
	id, who := caller(i)
	p, err := b.member(ctx, bd, id, who)
	if err != nil {
		b.failed(i, err)
		return
	}
	c := b.api.As(p.Token)
	t, err := c.AddTask(ctx, bd.ListID, title, details)
	if err != nil {
		b.failed(i, err)
		return
	}
	if forUser != nil {
		if forMember != nil {
			forMember.User = forUser
		}
		target, err := b.member(ctx, bd, forUser.ID, displayName(forMember, forUser))
		if err == nil {
			t, err = c.Assign(ctx, t.ID, target.DoablesID)
		}
		if err != nil {
			// The task exists; it is only nobody's yet, which the card says.
			b.log.Printf("discordbot: assigning new task %d: %v", t.ID, err)
		}
	}
	cd, err := b.postCard(ctx, bd, t, id)
	if err != nil {
		b.failed(i, err)
		return
	}
	b.answer(i, fmt.Sprintf("Added **%s**: %s", t.Title, messageLink(bd.GuildID, cd.ChannelID, cd.MessageID)))
}

func (b *Bot) cmdTasks(ctx context.Context, i *discordgo.Interaction) {
	bd, ok := b.boardHere(i)
	if !ok {
		return
	}
	b.thinking(i, true)
	id, who := caller(i)
	p, err := b.member(ctx, bd, id, who)
	if err != nil {
		b.failed(i, err)
		return
	}
	ts, err := b.api.As(p.Token).Tasks(ctx, bd.ListID, "todo")
	if err != nil {
		b.failed(i, err)
		return
	}
	var lines []string
	finished := 0
	for _, t := range ts {
		if t.Done {
			finished++
			continue
		}
		who := "anyone"
		if t.AssigneeID != 0 {
			who = b.mention(t.AssigneeID, t.Assignee)
		}
		lines = append(lines, fmt.Sprintf("• %s: %s", b.taskLink(t), who))
	}
	head := "**Still to do on this board**"
	if len(lines) == 0 {
		head = "Nothing is left to do on this board. 🎉"
	}
	if finished > 0 {
		head += fmt.Sprintf(" (and %d finished today)", finished)
	}
	b.answer(i, listing(head, lines))
}

func (b *Bot) cmdMine(ctx context.Context, i *discordgo.Interaction) {
	b.thinking(i, true)
	id, who := caller(i)
	p, err := b.personFor(ctx, id, who)
	if err != nil {
		b.failed(i, err)
		return
	}
	ts, err := b.api.As(p.Token).Mine(ctx)
	if err != nil {
		b.failed(i, err)
		return
	}
	if len(ts) == 0 {
		b.answer(i, "Nothing is yours right now. 🎉")
		return
	}
	var lines []string
	for _, t := range ts {
		lines = append(lines, fmt.Sprintf("• %s (%s)", b.taskLink(t), t.ListName))
	}
	b.answer(i, listing(fmt.Sprintf("**Yours: %d to do**", len(ts)), lines))
}

func (b *Bot) cmdDoables(ctx context.Context, i *discordgo.Interaction) {
	b.thinking(i, true)
	id, who := caller(i)
	p, err := b.personFor(ctx, id, who)
	if err != nil {
		b.failed(i, err)
		return
	}
	where := "your team's Doables server"
	if b.webURL != "" {
		where = "<" + b.webURL + ">"
	}
	b.answer(i, fmt.Sprintf("Open %s, choose **Already use Doables on another device?**, and paste this:\n||`%s`||\n"+
		"It is your sign-in for Doables, so keep it to yourself: anyone who has it can act as you.", where, p.Token))
}

// taskLink is a task's title, linking to its card when it has one.
func (b *Bot) taskLink(t store.Task) string {
	title := strings.NewReplacer("[", "(", "]", ")").Replace(t.Title)
	if c, ok, _ := b.state.card(t.ID); ok {
		if bd, ok, _ := b.state.boardByList(c.ListID); ok {
			return fmt.Sprintf("[%s](%s)", title, messageLink(bd.GuildID, c.ChannelID, c.MessageID))
		}
	}
	return title
}

// listing joins lines into one message, leaving off what does not fit in
// Discord's 2000 characters.
func listing(head string, lines []string) string {
	out := head
	for n, l := range lines {
		if utf8.RuneCountInString(out)+utf8.RuneCountInString(l)+40 > 2000 {
			return out + fmt.Sprintf("\n…and %d more", len(lines)-n)
		}
		out += "\n" + l
	}
	return out
}

// ---- buttons ----------------------------------------------------------

// cardFor finds the card and board a button belongs to.
func (b *Bot) cardFor(taskID int64) (card, board, error) {
	c, ok, err := b.state.card(taskID)
	if err != nil {
		return c, board{}, err
	}
	if !ok {
		return c, board{}, problem("I don't know that task any more.")
	}
	bd, ok, err := b.state.boardByList(c.ListID)
	if err == nil && !ok {
		err = problem("This board isn't set up any more.")
	}
	return c, bd, err
}

func (b *Bot) pressDone(ctx context.Context, i *discordgo.Interaction, taskID int64) {
	// Acknowledge first: the card itself is what changes, once Doables has.
	if err := b.dg.InteractionRespond(i, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate}); err != nil {
		b.log.Printf("discord: answering: %v", err)
	}
	c, bd, err := b.cardFor(taskID)
	if err != nil {
		b.followUp(i, err)
		return
	}
	unlock := b.lockBoard(bd.ListID)
	defer unlock()
	id, who := caller(i)
	p, err := b.member(ctx, bd, id, who)
	if err != nil {
		b.followUp(i, err)
		return
	}
	api := b.api.As(p.Token)
	t, found, err := findTask(ctx, api, bd.ListID, taskID)
	if err != nil {
		b.followUp(i, err)
		return
	}
	if !found {
		b.refresh(ctx, &c, store.Task{ID: taskID, ListID: bd.ListID, Title: cardTitle(i)}, true, id)
		return
	}
	if t, err = api.SetDone(ctx, taskID, !t.Done); err != nil {
		b.followUp(i, err)
		return
	}
	b.refresh(ctx, &c, t, false, id)
}

// pressAssign asks who the task is for, in a message only the presser sees.
func (b *Bot) pressAssign(i *discordgo.Interaction, taskID int64) {
	zero := 0
	id := strconv.FormatInt(taskID, 10)
	err := b.dg.InteractionRespond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags:   discordgo.MessageFlagsEphemeral,
			Content: fmt.Sprintf("Who should do **%s**?", cardTitle(i)),
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{
					MenuType: discordgo.UserSelectMenu, CustomID: "assignto:" + id,
					Placeholder: "Choose a person", MinValues: &zero, MaxValues: 1,
				}}},
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.Button{
					Label: "Nobody in particular", Style: discordgo.SecondaryButton, CustomID: "unassign:" + id,
				}}},
			},
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		},
	})
	if err != nil {
		b.log.Printf("discord: answering: %v", err)
	}
}

func (b *Bot) chooseAssignee(ctx context.Context, i *discordgo.Interaction, taskID int64, nobody bool) {
	data := i.MessageComponentData()
	if !nobody && len(data.Values) == 0 {
		nobody = true // they emptied the menu
	}
	var u *discordgo.User
	var m *discordgo.Member
	if !nobody {
		u, m = data.Resolved.Users[data.Values[0]], data.Resolved.Members[data.Values[0]]
		if u == nil {
			b.sayInstead(i, "I couldn't find that person. Press **Assign** and try again.")
			return
		}
		if u.Bot {
			b.sayInstead(i, "Bots can't do tasks. Choose a person instead.")
			return
		}
	}
	if err := b.dg.InteractionRespond(i, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate}); err != nil {
		b.log.Printf("discord: answering: %v", err)
	}
	c, bd, err := b.cardFor(taskID)
	if err != nil {
		b.failed(i, err)
		return
	}
	unlock := b.lockBoard(bd.ListID)
	defer unlock()
	id, who := caller(i)
	p, err := b.member(ctx, bd, id, who)
	if err != nil {
		b.failed(i, err)
		return
	}
	target := int64(0)
	if !nobody {
		tp, err := b.member(ctx, bd, u.ID, displayName(m, u))
		if err != nil {
			b.failed(i, err)
			return
		}
		target = tp.DoablesID
	}
	t, err := b.api.As(p.Token).Assign(ctx, taskID, target)
	if client.IsStatus(err, http.StatusNotFound) {
		b.answer(i, "That task has been deleted.")
		return
	}
	if err != nil {
		b.failed(i, err)
		return
	}
	b.refresh(ctx, &c, t, false, id)
	if nobody {
		b.answer(i, fmt.Sprintf("**%s** is anyone's now.", t.Title))
	} else {
		b.answer(i, fmt.Sprintf("**%s** is <@%s>'s now.", t.Title, u.ID))
	}
}

// sayInstead replaces the message a menu was in with some words.
func (b *Bot) sayInstead(i *discordgo.Interaction, text string) {
	if err := b.dg.InteractionRespond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{Content: text, Components: []discordgo.MessageComponent{}},
	}); err != nil {
		b.log.Printf("discord: answering: %v", err)
	}
}

// followUp tells someone privately why the button they pressed did nothing.
// The card is not theirs to edit with an error.
func (b *Bot) followUp(i *discordgo.Interaction, err error) {
	text := "Sorry, that didn't work: Doables could not be reached or said no. Please try again in a moment."
	var p problem
	if errors.As(err, &p) {
		text = string(p)
	} else {
		b.log.Printf("discordbot: %v", err)
	}
	if _, err := b.dg.FollowupMessageCreate(i, &discordgo.WebhookParams{Content: text, Flags: discordgo.MessageFlagsEphemeral,
		AllowedMentions: &discordgo.MessageAllowedMentions{}}); err != nil {
		b.log.Printf("discord: answering: %v", err)
	}
}

// cardTitle is the task's title as the card a button is on shows it.
func cardTitle(i *discordgo.Interaction) string {
	if i.Message != nil && len(i.Message.Embeds) > 0 {
		return plainTitle(i.Message.Embeds[0].Title)
	}
	return "this task"
}

func findTask(ctx context.Context, c *client.Client, listID, taskID int64) (store.Task, bool, error) {
	ts, err := c.Tasks(ctx, listID, "")
	if err != nil {
		return store.Task{}, false, err
	}
	for _, t := range ts {
		if t.ID == taskID {
			return t, true, nil
		}
	}
	return store.Task{}, false, nil
}

// ---- threads ----------------------------------------------------------

const maxComment = 500 // what Doables takes

// HandleMessage copies what people say in a task's thread to the task's
// comments in Doables, as them.
func (b *Bot) HandleMessage(m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot {
		return
	}
	c, ok, err := b.state.cardByThread(m.ChannelID)
	if err != nil || !ok {
		return
	}
	body := strings.TrimSpace(m.Content)
	if body == "" {
		return // a picture or a sticker: nothing Doables can hold
	}
	bd, ok, err := b.state.boardByList(c.ListID)
	if err != nil || !ok {
		return
	}
	if utf8.RuneCountInString(body) > maxComment {
		b.post(m.ChannelID, fmt.Sprintf("<@%s> that's longer than Doables takes (%d characters), so it's only here in Discord.", m.Author.ID, maxComment), m.Author.ID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	unlock := b.lockBoard(bd.ListID)
	defer unlock()
	member := m.Member
	if member != nil {
		member.User = m.Author
	}
	p, err := b.member(ctx, bd, m.Author.ID, displayName(member, m.Author))
	if err != nil {
		b.log.Printf("discordbot: comment from %s: %v", m.Author.ID, err)
		if errors.Is(err, errBusy) {
			b.post(m.ChannelID, fmt.Sprintf("<@%s> %s", m.Author.ID, errBusy), m.Author.ID)
		}
		return
	}
	cm, err := b.api.As(p.Token).AddComment(ctx, c.TaskID, body)
	if err != nil {
		b.log.Printf("discordbot: comment on task %d: %v", c.TaskID, err)
		return
	}
	// Recorded before the lock is let go, so the next look at the list does
	// not copy it straight back into the thread.
	if err := b.state.markMirrored(cm.ID); err != nil {
		b.log.Printf("discordbot: %v", err)
	}
}

// post says something in a channel or thread, pinging only those named.
func (b *Bot) post(channelID, text string, ping ...string) {
	if _, err := b.dg.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: text, AllowedMentions: &discordgo.MessageAllowedMentions{Users: ping},
	}); err != nil {
		b.log.Printf("discord: posting in %s: %v", channelID, err)
	}
}
