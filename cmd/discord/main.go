// Command doables-discord is a Discord bot that turns channels into task
// boards kept in a Doables server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/bwmarrin/discordgo"

	"doables/internal/client"
	"doables/internal/discordbot"
)

// permissions is what the bot needs in a board's channel: to see it, post and
// edit task cards (with embeds and links), and start and talk in their threads.
const permissions = discordgo.PermissionViewChannel |
	discordgo.PermissionSendMessages |
	discordgo.PermissionEmbedLinks |
	discordgo.PermissionReadMessageHistory |
	discordgo.PermissionCreatePublicThreads |
	discordgo.PermissionSendMessagesInThreads

func main() {
	server := flag.String("server", envOr("DOABLES_SERVER", "http://localhost:8080"), "the Doables server the bot talks to (env DOABLES_SERVER)")
	webURL := flag.String("url", os.Getenv("DOABLES_URL"), "the address people open Doables at, for links to it; leave out if they can't (env DOABLES_URL)")
	db := flag.String("db", envOr("DOABLES_DISCORD_DB", "doables-discord.db"), "the bot's own database; it holds everyone's Doables token, so keep it private (env DOABLES_DISCORD_DB)")
	guild := flag.String("guild", os.Getenv("DISCORD_GUILD"), "register commands in this one server only, where they appear at once (env DISCORD_GUILD)")
	flag.Parse()

	if err := run(*server, *webURL, *db, *guild); err != nil {
		log.Fatal(err)
	}
}

func run(server, webURL, db, guild string) error {
	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		return errors.New("set DISCORD_TOKEN to the bot's token, from the Discord Developer Portal (Bot → Reset Token)")
	}
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		return err
	}
	// Message content is a privileged intent: it must also be switched on in
	// the Developer Portal (Bot → Message Content Intent). It is what lets
	// replies in a task's thread become comments in Doables.
	dg.Identify.Intents = discordgo.IntentGuilds | discordgo.IntentGuildMessages | discordgo.IntentMessageContent

	api := client.New(server, "")
	if _, err := api.Lists(context.Background()); err != nil {
		return fmt.Errorf("can't reach Doables at %s: %w", server, err)
	}
	bot, err := discordbot.New(discordbot.Config{Discord: discordbot.FromSession(dg), Doables: api, StatePath: db, WebURL: webURL})
	if err != nil {
		return err
	}
	defer bot.Close()

	dg.AddHandler(func(_ *discordgo.Session, i *discordgo.InteractionCreate) { bot.HandleInteraction(i) })
	dg.AddHandler(func(_ *discordgo.Session, m *discordgo.MessageCreate) { bot.HandleMessage(m) })
	dg.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		if _, err := s.ApplicationCommandBulkOverwrite(r.Application.ID, guild, discordbot.Commands()); err != nil {
			log.Printf("registering commands: %v", err)
		}
		log.Printf("signed in to Discord as %s, in %d servers", r.User.Username, len(r.Guilds))
		log.Printf("to add it to a server: https://discord.com/oauth2/authorize?client_id=%s&scope=bot+applications.commands&permissions=%d",
			r.Application.ID, permissions)
	})
	if err := dg.Open(); err != nil {
		return fmt.Errorf("connecting to Discord: %w", err)
	}
	defer dg.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("keeping task boards in step with %s", server)
	bot.Run(ctx)
	log.Print("stopping")
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
