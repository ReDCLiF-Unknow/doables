package discordbot

import "github.com/bwmarrin/discordgo"

// FromSession is a real Discord session, as the bot uses it.
func FromSession(s *discordgo.Session) Discord { return session{s} }

type session struct{ s *discordgo.Session }

func (d session) InteractionRespond(i *discordgo.Interaction, r *discordgo.InteractionResponse) error {
	return d.s.InteractionRespond(i, r)
}

func (d session) InteractionResponseEdit(i *discordgo.Interaction, e *discordgo.WebhookEdit) (*discordgo.Message, error) {
	return d.s.InteractionResponseEdit(i, e)
}

func (d session) FollowupMessageCreate(i *discordgo.Interaction, p *discordgo.WebhookParams) (*discordgo.Message, error) {
	return d.s.FollowupMessageCreate(i, true, p)
}

func (d session) ChannelMessageSendComplex(channelID string, m *discordgo.MessageSend) (*discordgo.Message, error) {
	return d.s.ChannelMessageSendComplex(channelID, m)
}

func (d session) ChannelMessageEditComplex(m *discordgo.MessageEdit) (*discordgo.Message, error) {
	return d.s.ChannelMessageEditComplex(m)
}

func (d session) MessageThreadStartComplex(channelID, messageID string, t *discordgo.ThreadStart) (*discordgo.Channel, error) {
	return d.s.MessageThreadStartComplex(channelID, messageID, t)
}

func (d session) ChannelName(channelID string) string {
	if c, err := d.s.State.Channel(channelID); err == nil {
		return c.Name
	}
	if c, err := d.s.Channel(channelID); err == nil {
		return c.Name
	}
	return ""
}
