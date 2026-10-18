package discordbot

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"doables/internal/client"
	"doables/internal/store"
	"doables/internal/web"
)

// The bot is tested against a real Doables server, with a stand-in for
// Discord that remembers every message the bot posts and edits. A test plays
// the people on Discord by sending the bot what Discord would.

type msg struct {
	ID, Channel, Content string
	Embeds               []*discordgo.MessageEmbed
	Components           []discordgo.MessageComponent
	Pings                []string
}

type fakeDiscord struct {
	mu      sync.Mutex
	n       int
	msgs    map[string]*msg
	order   []string
	threads map[string]string   // thread ID -> the message it hangs off
	replies map[string][]string // interaction ID -> what the bot answered, in order
	edits   int                 // how many times a message was edited
}

func newFake() *fakeDiscord {
	return &fakeDiscord{msgs: map[string]*msg{}, threads: map[string]string{}, replies: map[string][]string{}}
}

func (f *fakeDiscord) id() string { f.n++; return fmt.Sprint(9000 + f.n) }

func (f *fakeDiscord) reply(i *discordgo.Interaction, text string) {
	f.replies[i.ID] = append(f.replies[i.ID], text)
}

func (f *fakeDiscord) InteractionRespond(i *discordgo.Interaction, r *discordgo.InteractionResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Data != nil && r.Data.Content != "" {
		f.reply(i, r.Data.Content)
	}
	return nil
}

func (f *fakeDiscord) InteractionResponseEdit(i *discordgo.Interaction, e *discordgo.WebhookEdit) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e.Content != nil {
		f.reply(i, *e.Content)
	}
	return &discordgo.Message{}, nil
}

func (f *fakeDiscord) FollowupMessageCreate(i *discordgo.Interaction, p *discordgo.WebhookParams) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply(i, p.Content)
	return &discordgo.Message{}, nil
}

func (f *fakeDiscord) ChannelMessageSendComplex(channelID string, m *discordgo.MessageSend) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	nm := &msg{ID: f.id(), Channel: channelID, Content: m.Content, Embeds: m.Embeds, Components: m.Components}
	if m.AllowedMentions != nil {
		nm.Pings = m.AllowedMentions.Users
	}
	f.msgs[nm.ID] = nm
	f.order = append(f.order, nm.ID)
	return &discordgo.Message{ID: nm.ID, ChannelID: channelID}, nil
}

func (f *fakeDiscord) ChannelMessageEditComplex(e *discordgo.MessageEdit) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits++
	m, ok := f.msgs[e.ID]
	if !ok {
		return nil, &discordgo.RESTError{Message: &discordgo.APIErrorMessage{Code: discordgo.ErrCodeUnknownMessage}}
	}
	if e.Embeds != nil {
		m.Embeds = *e.Embeds
	}
	if e.Components != nil {
		m.Components = *e.Components
	}
	return &discordgo.Message{ID: m.ID}, nil
}

func (f *fakeDiscord) MessageThreadStartComplex(channelID, messageID string, t *discordgo.ThreadStart) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "t" + f.id()
	f.threads[id] = messageID
	return &discordgo.Channel{ID: id, Name: t.Name}, nil
}

func (f *fakeDiscord) ChannelName(string) string { return "staff-tasks" }

// card finds the message that shows a task.
func (f *fakeDiscord) card(t *testing.T, taskID int64) *msg {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var found *msg
	for _, id := range f.order {
		m := f.msgs[id]
		if m != nil && len(m.Embeds) > 0 && m.Embeds[0].Footer != nil && m.Embeds[0].Footer.Text == fmt.Sprintf("Task %d", taskID) {
			if found != nil {
				t.Fatalf("task %d has two cards", taskID)
			}
			found = m
		}
	}
	if found == nil {
		t.Fatalf("task %d has no card", taskID)
	}
	return found
}

// thread is what has been said in the thread under a message.
func (f *fakeDiscord) thread(messageID string) []*msg {
	f.mu.Lock()
	defer f.mu.Unlock()
	var tid string
	for id, m := range f.threads {
		if m == messageID {
			tid = id
		}
	}
	var out []*msg
	for _, id := range f.order {
		if m := f.msgs[id]; m != nil && m.Channel == tid && tid != "" {
			out = append(out, f.msgs[id])
		}
	}
	return out
}

func (f *fakeDiscord) threadOf(messageID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, m := range f.threads {
		if m == messageID {
			return id
		}
	}
	return ""
}

func (f *fakeDiscord) answers(i *discordgo.InteractionCreate) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.replies[i.ID], "\n")
}

// ---- the setting --------------------------------------------------------

type env struct {
	t   *testing.T
	bot *Bot
	dc  *fakeDiscord
	api *client.Client
	n   int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "doables.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(web.New(s))
	t.Cleanup(func() { srv.Close(); s.Close() })
	dc := newFake()
	api := client.New(srv.URL, "")
	b, err := New(Config{Discord: dc, Doables: api, StatePath: filepath.Join(t.TempDir(), "bot.db"),
		WebURL: "https://tasks.example", Log: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return &env{t: t, bot: b, dc: dc, api: api}
}

var (
	alex = &discordgo.Member{User: &discordgo.User{ID: "101", Username: "alex_x"}, Nick: "Alex"}
	sam  = &discordgo.Member{User: &discordgo.User{ID: "102", Username: "sam99", GlobalName: "Sam"}}
	kim  = &discordgo.Member{User: &discordgo.User{ID: "103", Username: "kim"}}
	robo = &discordgo.Member{User: &discordgo.User{ID: "199", Username: "robo", Bot: true}}
)

const boardChannel = "500"

func (e *env) interaction(by *discordgo.Member, typ discordgo.InteractionType, data discordgo.InteractionData) *discordgo.InteractionCreate {
	e.n++
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: fmt.Sprint("i", e.n), Type: typ, GuildID: "1", ChannelID: boardChannel, Member: by, Data: data,
	}}
}

func str(name, v string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionString, Value: v}
}

// command runs a slash command and returns what the bot answered.
func (e *env) command(by *discordgo.Member, name string, opts ...*discordgo.ApplicationCommandInteractionDataOption) string {
	e.t.Helper()
	return e.commandFor(by, name, nil, opts...)
}

// commandFor is command with a person chosen in its "for" option.
func (e *env) commandFor(by *discordgo.Member, name string, forWho *discordgo.Member, opts ...*discordgo.ApplicationCommandInteractionDataOption) string {
	e.t.Helper()
	data := discordgo.ApplicationCommandInteractionData{Name: name, Options: opts}
	if forWho != nil {
		data.Options = append(data.Options, &discordgo.ApplicationCommandInteractionDataOption{
			Name: "for", Type: discordgo.ApplicationCommandOptionUser, Value: forWho.User.ID})
		data.Resolved = &discordgo.ApplicationCommandInteractionDataResolved{
			Users:   map[string]*discordgo.User{forWho.User.ID: forWho.User},
			Members: map[string]*discordgo.Member{forWho.User.ID: {Nick: forWho.Nick}},
		}
	}
	i := e.interaction(by, discordgo.InteractionApplicationCommand, data)
	e.bot.HandleInteraction(i)
	return e.dc.answers(i)
}

// press presses a button on a card.
func (e *env) press(by *discordgo.Member, card *msg, customID string) string {
	e.t.Helper()
	i := e.interaction(by, discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: customID})
	i.Message = &discordgo.Message{ID: card.ID, Embeds: card.Embeds}
	e.bot.HandleInteraction(i)
	return e.dc.answers(i)
}

// choose picks someone in the Assign menu, or nobody.
func (e *env) choose(by *discordgo.Member, taskID int64, who *discordgo.Member) string {
	e.t.Helper()
	data := discordgo.MessageComponentInteractionData{CustomID: fmt.Sprint("unassign:", taskID)}
	if who != nil {
		data = discordgo.MessageComponentInteractionData{CustomID: fmt.Sprint("assignto:", taskID),
			Values: []string{who.User.ID},
			Resolved: discordgo.MessageComponentInteractionDataResolved{
				Users:   map[string]*discordgo.User{who.User.ID: who.User},
				Members: map[string]*discordgo.Member{who.User.ID: {Nick: who.Nick}},
			}}
	}
	i := e.interaction(by, discordgo.InteractionMessageComponent, data)
	e.bot.HandleInteraction(i)
	return e.dc.answers(i)
}

// say writes in a thread.
func (e *env) say(by *discordgo.Member, threadID, text string) {
	e.bot.HandleMessage(&discordgo.MessageCreate{Message: &discordgo.Message{
		ChannelID: threadID, GuildID: "1", Author: by.User, Member: &discordgo.Member{Nick: by.Nick}, Content: text,
	}})
}

func (e *env) sync() {
	e.t.Helper()
	e.bot.syncAll(context.Background(), "")
}

// owner is the Doables client of whoever set up the board.
func (e *env) as(m *discordgo.Member) *client.Client {
	e.t.Helper()
	p, ok, err := e.bot.state.person(m.User.ID)
	if err != nil || !ok {
		e.t.Fatalf("%s has no Doables identity", m.User.Username)
	}
	return e.api.As(p.Token)
}

func (e *env) board() board {
	e.t.Helper()
	bd, ok, err := e.bot.state.board(boardChannel)
	if err != nil || !ok {
		e.t.Fatal("no board")
	}
	return bd
}

func (e *env) onlyTask() store.Task {
	e.t.Helper()
	ts, err := e.as(alex).Tasks(context.Background(), e.board().ListID, "")
	if err != nil || len(ts) != 1 {
		e.t.Fatalf("want one task, got %d (%v)", len(ts), err)
	}
	return ts[0]
}

func has(t *testing.T, what, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("%s: want %q in:\n%s", what, want, got)
	}
}

func lacks(t *testing.T, what, got, unwanted string) {
	t.Helper()
	if strings.Contains(got, unwanted) {
		t.Errorf("%s: did not want %q in:\n%s", what, unwanted, got)
	}
}

// field is what a card says under a heading, as in "For".
func field(m *msg, name string) string {
	for _, f := range m.Embeds[0].Fields {
		if f.Name == name {
			return f.Value
		}
	}
	return ""
}

func buttons(m *msg) string {
	var labels []string
	for _, row := range m.Components {
		for _, c := range row.(discordgo.ActionsRow).Components {
			labels = append(labels, c.(discordgo.Button).Label)
		}
	}
	return strings.Join(labels, ", ")
}

// ---- tests --------------------------------------------------------------

func TestSettingUpABoard(t *testing.T) {
	e := newEnv(t)
	has(t, "/task before setup", e.command(alex, "task", str("title", "x")), "isn't a task board")

	out := e.command(alex, "taskboard")
	has(t, "/taskboard", out, "now a task board")
	has(t, "/taskboard names the list after the channel", out, "**staff-tasks**")
	has(t, "a second /taskboard", e.command(sam, "taskboard"), "already a task board")

	ls, err := e.as(alex).Lists(context.Background())
	if err != nil || len(ls) != 1 || ls[0].Name != "staff-tasks" {
		t.Fatalf("Alex's lists in Doables: %+v (%v)", ls, err)
	}
	me, _ := e.as(alex).Me(context.Background())
	if me.Name != "Alex" {
		t.Errorf("Alex is called %q in Doables, want their server nickname", me.Name)
	}
}

func TestAddingATaskForSomeone(t *testing.T) {
	e := newEnv(t)
	e.command(alex, "taskboard", str("name", "Front desk"))
	out := e.commandFor(alex, "task", sam, str("title", "Restock the leaflets"), str("details", "Top shelf"))
	has(t, "/task", out, "Added **Restock the leaflets**")
	has(t, "/task links to the card", out, "https://discord.com/channels/1/500/")

	task := e.onlyTask()
	if task.Assignee != "Sam" || task.AddedBy != "Alex" || task.Description != "Top shelf" {
		t.Errorf("in Doables: %+v", task)
	}
	c := e.dc.card(t, task.ID)
	has(t, "card says whose it is", field(c, "For"), "<@102>")
	has(t, "card says who added it", field(c, "Added by"), "Alex")
	has(t, "card links to Doables", c.Embeds[0].URL, "https://tasks.example/lists/")
	has(t, "card buttons", buttons(c), "Done, Assign, Open in Doables")

	th := e.dc.thread(c.ID)
	if len(th) != 1 || th[0].Pings[0] != "102" {
		t.Fatalf("Sam should be pinged once in the thread: %+v", th)
	}
	has(t, "the ping", th[0].Content, "<@101> has given this one to you")

	// Nothing changed, so a look at the board does nothing.
	e.sync()
	if got := len(e.dc.thread(c.ID)); got != 1 {
		t.Errorf("a sync with nothing new posted in the thread: %d messages", got)
	}

	has(t, "bots get no tasks", e.commandFor(alex, "task", robo, str("title", "Beep")), "Bots can't do tasks")
}

func TestDoneButton(t *testing.T) {
	e := newEnv(t)
	e.command(alex, "taskboard")
	e.command(alex, "task", str("title", "Lock up"))
	task := e.onlyTask()

	// Kim has never used the bot: pressing Done makes them somebody, on the list.
	e.press(kim, e.dc.card(t, task.ID), fmt.Sprint("done:", task.ID))
	task = e.onlyTask()
	if !task.Done || task.DoneBy != "kim" {
		t.Fatalf("after Done: %+v", task)
	}
	c := e.dc.card(t, task.ID)
	has(t, "done card", c.Embeds[0].Title, "✅ Lock up")
	has(t, "done card", field(c, "Done by"), "kim")
	has(t, "done card", buttons(c), "Not done yet")

	e.press(sam, c, fmt.Sprint("done:", task.ID))
	if e.onlyTask().Done {
		t.Fatal("pressing again should reopen it")
	}
	has(t, "reopened card", buttons(e.dc.card(t, task.ID)), "Done, Assign")
}

func TestAssigning(t *testing.T) {
	e := newEnv(t)
	e.command(alex, "taskboard")
	e.command(alex, "task", str("title", "Order paper"))
	task := e.onlyTask()
	c := e.dc.card(t, task.ID)

	has(t, "Assign asks", e.press(alex, c, fmt.Sprint("assign:", task.ID)), "Who should do **Order paper**?")
	has(t, "choosing Kim", e.choose(alex, task.ID, kim), "is <@103>'s now")
	if got := e.onlyTask().Assignee; got != "kim" {
		t.Fatalf("assignee in Doables: %q", got)
	}
	has(t, "card", field(e.dc.card(t, task.ID), "For"), "<@103>")
	th := e.dc.thread(c.ID)
	if len(th) != 1 || len(th[0].Pings) != 1 || th[0].Pings[0] != "103" {
		t.Fatalf("Kim should be pinged: %+v", th)
	}

	// Taking it yourself pings nobody.
	e.choose(sam, task.ID, sam)
	if got := len(e.dc.thread(c.ID)); got != 1 {
		t.Errorf("Sam took it themself and was pinged anyway: %d thread messages", got)
	}

	has(t, "nobody", e.choose(alex, task.ID, nil), "anyone's now")
	has(t, "card", field(e.dc.card(t, task.ID), "For"), "Anyone")
	has(t, "bots", e.choose(alex, task.ID, robo), "Bots can't do tasks")
}

func TestThreadsAreComments(t *testing.T) {
	e := newEnv(t)
	e.command(alex, "taskboard")
	e.command(alex, "task", str("title", "Fix the printer"))
	task := e.onlyTask()
	c := e.dc.card(t, task.ID)
	thread := e.dc.threadOf(c.ID)

	e.say(sam, thread, "It needs toner, I'll get some")
	cs, err := e.as(alex).Comments(context.Background(), task.ID)
	if err != nil || len(cs) != 1 || cs[0].Author != "Sam" || cs[0].Body != "It needs toner, I'll get some" {
		t.Fatalf("comments in Doables: %+v (%v)", cs, err)
	}

	// Someone answers in the web app; the bot copies it in, and does not copy
	// Sam's own comment back.
	if _, err := e.as(alex).AddComment(context.Background(), task.ID, "Thanks!"); err != nil {
		t.Fatal(err)
	}
	e.sync()
	e.sync()
	var said []string
	for _, m := range e.dc.thread(c.ID) {
		said = append(said, m.Content)
	}
	got := strings.Join(said, "\n")
	has(t, "thread", got, "**Alex** in Doables:\nThanks!")
	lacks(t, "thread", got, "toner")
	if len(said) != 1 {
		t.Errorf("thread should have one copied comment: %q", said)
	}

	e.say(sam, thread, strings.Repeat("a", 501))
	has(t, "too long", e.dc.thread(c.ID)[1].Content, "longer than Doables takes")
	e.say(robo, thread, "bots are ignored")
	if cs, _ := e.as(alex).Comments(context.Background(), task.ID); len(cs) != 2 {
		t.Errorf("want 2 comments in Doables, got %d", len(cs))
	}
}

func TestChangesMadeInTheWebApp(t *testing.T) {
	e := newEnv(t)
	e.command(alex, "taskboard")
	e.command(alex, "task", str("title", "Water the plants"))
	first := e.onlyTask()
	ctx := context.Background()
	owner := e.as(alex)

	// A task added in Doables gets a card; one ticked off there shows as done.
	added, err := owner.AddTask(ctx, e.board().ListID, "Empty the bins", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.SetDone(ctx, first.ID, true); err != nil {
		t.Fatal(err)
	}
	e.sync()
	has(t, "web-added task", e.dc.card(t, added.ID).Embeds[0].Title, "Empty the bins")
	has(t, "web-ticked task", e.dc.card(t, first.ID).Embeds[0].Title, "✅")

	// Looking again with nothing changed edits nothing, so no card says
	// "(edited)" for no reason.
	e.dc.mu.Lock()
	before := e.dc.edits
	e.dc.mu.Unlock()
	e.sync()
	e.dc.mu.Lock()
	if e.dc.edits != before {
		t.Errorf("a sync with nothing new made %d edits", e.dc.edits-before)
	}
	e.dc.mu.Unlock()

	// Deleted in Doables: the card stays, so its thread makes sense, but says
	// so and loses its buttons. Undo brings it back.
	if err := owner.DeleteTask(ctx, added.ID); err != nil {
		t.Fatal(err)
	}
	e.sync()
	c := e.dc.card(t, added.ID)
	has(t, "deleted card", c.Embeds[0].Title, "🗑️ Empty the bins")
	if len(c.Components) != 0 {
		t.Errorf("a deleted task's card still has buttons: %s", buttons(c))
	}

	// Someone deletes a card in Discord; the task is still there, so the next
	// look puts it back.
	gone := e.dc.card(t, first.ID).ID
	e.dc.mu.Lock()
	delete(e.dc.msgs, gone)
	e.dc.mu.Unlock()
	if _, err := owner.SetDone(ctx, first.ID, false); err != nil {
		t.Fatal(err)
	}
	e.sync()
	has(t, "card put back", e.dc.card(t, first.ID).Embeds[0].Title, "Water the plants")
}

func TestListsOfTasks(t *testing.T) {
	e := newEnv(t)
	e.command(alex, "taskboard")
	e.commandFor(alex, "task", sam, str("title", "Count the till"))
	e.command(alex, "task", str("title", "Sweep up"))

	out := e.command(kim, "tasks")
	has(t, "/tasks", out, "Still to do")
	has(t, "/tasks links to cards", out, "[Count the till](https://discord.com/channels/1/500/")
	has(t, "/tasks says whose", out, "<@102>")
	has(t, "/tasks", out, "Sweep up](")

	has(t, "/mine for Sam", e.command(sam, "mine"), "[Count the till](")
	has(t, "/mine for Kim", e.command(kim, "mine"), "Nothing is yours")

	out = e.command(sam, "doables")
	p, _, _ := e.bot.state.person(sam.User.ID)
	has(t, "/doables gives the token", out, "||`"+p.Token+"`||")
	has(t, "/doables says where", out, "<https://tasks.example>")
}

func TestLiveUpdates(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.bot.Run(ctx)
	e.command(alex, "taskboard") // starts following Alex's live updates

	// Added in the web app, and on the board moments later, well before the
	// five-minute look at every board.
	time.Sleep(200 * time.Millisecond) // let the stream connect
	added, err := e.as(alex).AddTask(ctx, e.board().ListID, "Call the plumber", "")
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !e.dc.hasCard(added.ID); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a task added in Doables did not reach the board")
		}
	}
}

func (f *fakeDiscord) hasCard(taskID int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.msgs {
		if len(m.Embeds) > 0 && m.Embeds[0].Footer != nil && m.Embeds[0].Footer.Text == fmt.Sprintf("Task %d", taskID) {
			return true
		}
	}
	return false
}

func TestTooManyNewPeople(t *testing.T) {
	e := newEnv(t)
	// The server lets one address make ten people in a row; the bot is one
	// address, so it says to try again rather than failing silently.
	for n := 0; n < 10; n++ {
		if _, _, err := e.api.Register(context.Background(), fmt.Sprint("someone ", n)); err != nil {
			t.Fatal(err)
		}
	}
	has(t, "/mine", e.command(alex, "mine"), "try again in a minute")
	if _, ok, _ := e.bot.state.person(alex.User.ID); ok {
		t.Error("a failed sign-up was remembered")
	}
}
