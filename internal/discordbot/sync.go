package discordbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"

	"doables/internal/client"
	"doables/internal/store"
)

// A board can change outside Discord: in the web app, from the CLI, or by its
// tasks being deleted. Each board owner's live updates are followed, and when
// anything changes their boards are looked at again and the cards brought up
// to date. Every so often they are looked at anyway, in case an update was
// missed while the connection was down.
const resyncEvery = 5 * time.Minute

// Run keeps the boards in step with Doables until ctx ends.
func (b *Bot) Run(ctx context.Context) {
	b.watchMu.Lock()
	b.runCtx = ctx
	b.watchMu.Unlock()

	bs, err := b.state.boards()
	if err != nil {
		b.log.Printf("discordbot: %v", err)
	}
	for _, bd := range bs {
		b.watch(bd.Owner)
	}
	tick := time.NewTicker(resyncEvery)
	defer tick.Stop()
	for {
		b.syncAll(ctx, "")
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// watch follows an owner's live updates, unless that is already happening or
// Run has not started.
func (b *Bot) watch(owner string) {
	b.watchMu.Lock()
	defer b.watchMu.Unlock()
	if b.runCtx == nil || b.watching[owner] {
		return
	}
	b.watching[owner] = true
	go b.follow(b.runCtx, owner)
}

func (b *Bot) follow(ctx context.Context, owner string) {
	p, ok, err := b.state.person(owner)
	if err != nil || !ok {
		b.log.Printf("discordbot: no Doables identity for board owner %s: %v", owner, err)
		return
	}
	// Bursts of changes become one look, a moment after the first.
	poke := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-poke:
			}
			time.Sleep(time.Second)
			select {
			case <-poke:
			default:
			}
			b.syncAll(ctx, owner)
		}
	}()
	wait := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := b.api.As(p.Token).Events(ctx, func() {
			select {
			case poke <- struct{}{}:
			default:
			}
		})
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			wait = time.Second // it was working; reconnect promptly
		}
		b.log.Printf("discordbot: live updates for %s stopped (%v); reconnecting in %s", owner, err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		// Whatever changed while disconnected is caught on reconnecting.
		poke <- struct{}{}
		wait = min(wait*2, time.Minute)
	}
}

// syncAll looks at every board, or only one owner's.
func (b *Bot) syncAll(ctx context.Context, owner string) {
	bs, err := b.state.boards()
	if err != nil {
		b.log.Printf("discordbot: %v", err)
		return
	}
	for _, bd := range bs {
		if owner != "" && bd.Owner != owner {
			continue
		}
		if err := b.syncBoard(ctx, bd); err != nil {
			b.log.Printf("discordbot: board %d: %v", bd.ListID, err)
		}
	}
}

// syncBoard brings one board's cards up to date with its list in Doables.
func (b *Bot) syncBoard(ctx context.Context, bd board) error {
	unlock := b.lockBoard(bd.ListID)
	defer unlock()

	owner, ok, err := b.state.person(bd.Owner)
	if err != nil || !ok {
		return fmt.Errorf("no owner: %v", err)
	}
	api := b.api.As(owner.Token)
	tasks, err := api.Tasks(ctx, bd.ListID, "")
	if client.IsStatus(err, http.StatusNotFound) {
		return errors.New("the list is gone from Doables, or its owner has left it")
	}
	if err != nil {
		return err
	}
	cards, err := b.state.cards(bd.ListID)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		c, ok := cards[t.ID]
		delete(cards, t.ID)
		if !ok {
			// Added outside Discord. Tasks already finished when the board
			// first sees them have nothing left to ask of anyone.
			if t.Done {
				continue
			}
			if _, err := b.postCard(ctx, bd, t, ""); err != nil {
				return err
			}
			continue
		}
		b.refresh(ctx, &c, t, false, "")
		if t.Comments != c.Comments {
			b.copyComments(ctx, api, &c)
		}
	}
	// What is left was deleted. It can be brought back for a day, and then it
	// is simply listed again.
	for _, c := range cards {
		if !c.Gone {
			b.refresh(ctx, &c, store.Task{ID: c.TaskID, ListID: c.ListID, Title: lastTitle(c)}, true, "")
		}
	}
	return nil
}

// postCard puts a task on its board, with a thread under it for talking about
// it. by is the Discord user who added it, if it was added from Discord.
func (b *Bot) postCard(ctx context.Context, bd board, t store.Task, by string) (card, error) {
	v := b.render(t, false)
	m, err := b.dg.ChannelMessageSendComplex(bd.ChannelID, &discordgo.MessageSend{
		Embeds: v.Embeds, Components: v.Components, AllowedMentions: &discordgo.MessageAllowedMentions{},
	})
	if err != nil {
		return card{}, fmt.Errorf("posting task %d: %w", t.ID, err)
	}
	c := card{TaskID: t.ID, ListID: bd.ListID, ChannelID: bd.ChannelID, MessageID: m.ID,
		Render: v.key(), Assignee: t.AssigneeID, Comments: t.Comments}
	th, err := b.dg.MessageThreadStartComplex(bd.ChannelID, m.ID, &discordgo.ThreadStart{
		Name: threadName(t.Title), AutoArchiveDuration: 10080, // a week of quiet
	})
	if err != nil {
		b.log.Printf("discordbot: no thread for task %d: %v", t.ID, err)
	} else {
		c.ThreadID = th.ID
	}
	if err := b.state.saveCard(c); err != nil {
		return c, err
	}
	if t.AssigneeID != 0 {
		b.tellAssignee(c, t.AssigneeID, by)
	}
	if t.Comments > 0 {
		b.copyComments(ctx, b.api.As(b.ownerToken(bd)), &c)
	}
	return c, nil
}

// refresh shows a task's current state on its card, if that has changed, and
// pings whoever it has just been given to. by is the Discord user whose doing
// that was, if it was one: nobody needs telling about what they just did.
func (b *Bot) refresh(ctx context.Context, c *card, t store.Task, gone bool, by string) {
	v := b.render(t, gone)
	if key := v.key(); key != c.Render {
		edit := discordgo.NewMessageEdit(c.ChannelID, c.MessageID)
		edit.Embeds = &v.Embeds
		comps := v.Components
		if comps == nil {
			comps = []discordgo.MessageComponent{}
		}
		edit.Components = &comps
		edit.AllowedMentions = &discordgo.MessageAllowedMentions{}
		if _, err := b.dg.ChannelMessageEditComplex(edit); err != nil {
			var re *discordgo.RESTError
			if errors.As(err, &re) && re.Message != nil && re.Message.Code == discordgo.ErrCodeUnknownMessage && !gone {
				// Somebody deleted the card in Discord, but the task is still
				// there: put it back, so the board shows everything.
				if bd, ok, _ := b.state.boardByList(c.ListID); ok {
					if nc, err := b.postCard(ctx, bd, t, by); err == nil {
						*c = nc
					}
				}
				return
			}
			b.log.Printf("discordbot: updating task %d's card: %v", t.ID, err)
			return
		}
		c.Render = key
	}
	if !gone && t.AssigneeID != c.Assignee && t.AssigneeID != 0 {
		b.tellAssignee(*c, t.AssigneeID, by)
	}
	if !gone {
		c.Assignee = t.AssigneeID
	}
	c.Gone = gone
	if err := b.state.saveCard(*c); err != nil {
		b.log.Printf("discordbot: %v", err)
	}
}

// tellAssignee pings someone in a task's thread when it has been given to
// them, which also adds them to the thread.
func (b *Bot) tellAssignee(c card, doablesID int64, by string) {
	who := b.state.discordID(doablesID)
	if who == "" || who == by || c.ThreadID == "" {
		return
	}
	text := fmt.Sprintf("<@%s> this one is yours now.", who)
	if by != "" {
		text = fmt.Sprintf("<@%s> <@%s> has given this one to you.", who, by)
	}
	b.post(c.ThreadID, text, who)
}

// copyComments posts into a task's thread the comments that were written in
// Doables and are not there yet.
func (b *Bot) copyComments(ctx context.Context, api *client.Client, c *card) {
	if c.ThreadID == "" {
		return
	}
	cs, err := api.Comments(ctx, c.TaskID)
	if err != nil {
		b.log.Printf("discordbot: comments on task %d: %v", c.TaskID, err)
		return
	}
	for _, cm := range cs {
		if b.state.mirrored(cm.ID) {
			continue
		}
		who := cm.Author
		if who == "" {
			who = "Someone who has left"
		}
		if _, err := b.dg.ChannelMessageSendComplex(c.ThreadID, &discordgo.MessageSend{
			Content:         fmt.Sprintf("**%s** in Doables:\n%s", who, cm.Body),
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		}); err != nil {
			b.log.Printf("discordbot: copying comment %d: %v", cm.ID, err)
			return // try again next time
		}
		if err := b.state.markMirrored(cm.ID); err != nil {
			b.log.Printf("discordbot: %v", err)
		}
	}
	c.Comments = len(cs)
	if err := b.state.saveCard(*c); err != nil {
		b.log.Printf("discordbot: %v", err)
	}
}

func (b *Bot) ownerToken(bd board) string {
	p, _, _ := b.state.person(bd.Owner)
	return p.Token
}

// lastTitle recovers a deleted task's title from what its card last showed,
// as Doables no longer says.
func lastTitle(c card) string {
	// Only the embeds: buttons are interfaces, which JSON cannot fill in.
	var v struct{ Embeds []*discordgo.MessageEmbed }
	if err := json.Unmarshal([]byte(c.Render), &v); err == nil && len(v.Embeds) > 0 {
		return plainTitle(v.Embeds[0].Title)
	}
	return fmt.Sprintf("Task %d", c.TaskID)
}
