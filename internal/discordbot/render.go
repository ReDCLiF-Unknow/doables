package discordbot

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"doables/internal/store"
)

// Tabler's colours, so a card looks like the same app as the web page.
const (
	colourOpen = 0x206bc4
	colourDone = 0x2fb344
	colourGone = 0x6c7a91
)

// view is what one task's card shows.
type view struct {
	Embeds     []*discordgo.MessageEmbed
	Components []discordgo.MessageComponent
}

// key is a fingerprint of a view, stored to tell whether a card needs editing.
func (v view) key() string {
	b, _ := json.Marshal(v)
	return string(b)
}

// render draws a task's card. gone is for a task that has been deleted: the
// card stays, so the thread under it still makes sense, but loses its buttons.
func (b *Bot) render(t store.Task, gone bool) view {
	e := &discordgo.MessageEmbed{
		Title:       t.Title,
		Description: t.Description,
		Color:       colourOpen,
		Footer:      &discordgo.MessageEmbedFooter{Text: "Task " + strconv.FormatInt(t.ID, 10)},
	}
	if b.webURL != "" {
		e.URL = fmt.Sprintf("%s/lists/%d", b.webURL, t.ListID)
	}
	if gone {
		e.Title = "🗑️ " + t.Title
		e.Description = "*Deleted in Doables.*"
		e.Color = colourGone
		return view{Embeds: []*discordgo.MessageEmbed{e}}
	}

	forWho := "Anyone: press **Assign** to give it to somebody"
	if t.AssigneeID != 0 {
		forWho = b.mention(t.AssigneeID, t.Assignee)
	}
	e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "For", Value: forWho, Inline: true})
	if t.AddedBy != "" {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "Added by", Value: t.AddedBy, Inline: true})
	}
	if t.DueDate != "" {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "Due", Value: t.DueDate, Inline: true})
	}

	done := discordgo.Button{Label: "Done", Style: discordgo.SuccessButton,
		Emoji: &discordgo.ComponentEmoji{Name: "✅"}, CustomID: "done:" + strconv.FormatInt(t.ID, 10)}
	if t.Done {
		e.Title = "✅ " + t.Title
		e.Color = colourDone
		if t.DoneBy != "" {
			e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "Done by", Value: t.DoneBy, Inline: true})
		}
		done = discordgo.Button{Label: "Not done yet", Style: discordgo.SecondaryButton,
			Emoji: &discordgo.ComponentEmoji{Name: "↩️"}, CustomID: done.CustomID}
	}
	row := discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		done,
		discordgo.Button{Label: "Assign", Style: discordgo.SecondaryButton,
			Emoji: &discordgo.ComponentEmoji{Name: "👤"}, CustomID: "assign:" + strconv.FormatInt(t.ID, 10)},
	}}
	if e.URL != "" {
		row.Components = append(row.Components, discordgo.Button{Label: "Open in Doables", Style: discordgo.LinkButton, URL: e.URL})
	}
	return view{Embeds: []*discordgo.MessageEmbed{e}, Components: []discordgo.MessageComponent{row}}
}

// mention names a Doables person the Discord way when they are on Discord,
// which shows as their name without pinging them, and by name otherwise.
func (b *Bot) mention(doablesID int64, name string) string {
	if id := b.state.discordID(doablesID); id != "" {
		return "<@" + id + ">"
	}
	return name
}

// plainTitle is a card's title without the mark render puts in front of it.
func plainTitle(s string) string {
	return strings.TrimPrefix(strings.TrimPrefix(s, "✅ "), "🗑️ ")
}

// threadName is what a task's discussion thread is called. Discord allows 100
// characters.
func threadName(title string) string {
	return clip(title, 100)
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// messageLink is the address of a Discord message, which opens it when clicked.
func messageLink(guildID, channelID, messageID string) string {
	return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", guildID, channelID, messageID)
}
