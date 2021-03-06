package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/mattermost/mattermost-server/v5/model"
	"github.com/mattermost/mattermost-server/v5/plugin"
	"github.com/pkg/errors"
)

type Plugin struct {
	plugin.MattermostPlugin

	// configurationLock synchronizes access to the configuration.
	configurationLock sync.RWMutex

	// configuration is the active plugin configuration. Consult getConfiguration and
	// setConfiguration for usage.
	configuration *configuration

	TeamID    string
	ChannelID string
	BotUserID string
}

const (
	SNS_ICON_URL = "https://cdn2.iconfinder.com/data/icons/amazon-aws-stencils/100/App_Services_copy_Amazon_SNS-512.png"
	SNS_USERNAME = "AWS SNS Bot"
)

func (p *Plugin) OnActivate() error {
	configuration := p.getConfiguration()

	if err := p.IsValid(configuration); err != nil {
		return err
	}

	split := strings.Split(p.configuration.TeamChannel, ",")
	if len(split) != 2 {
		return errors.New("teamChannel setting doesn't follow the pattern $TEAM_NAME,$CHANNEL_NAME")
	}

	teamSplit := split[0]
	channelSplit := split[1]

	team, err := p.API.GetTeamByName(teamSplit)
	if err != nil {
		return err
	}
	p.TeamID = team.Id

	user, err := p.API.GetUserByUsername(p.configuration.Username)
	if err != nil {
		p.API.LogError(err.Error())
		return fmt.Errorf("Unable to find user with configured username: %v", p.configuration.Username)
	}
	p.BotUserID = user.Id

	channel, err := p.API.GetChannelByName(team.Id, channelSplit, false)
	if err != nil && err.StatusCode == http.StatusNotFound {
		channelToCreate := &model.Channel{
			Name:        channelSplit,
			DisplayName: channelSplit,
			Type:        model.CHANNEL_OPEN,
			TeamId:      p.TeamID,
			CreatorId:   p.BotUserID,
		}

		newChannel, errChannel := p.API.CreateChannel(channelToCreate)
		if errChannel != nil {
			return errChannel
		}
		p.ChannelID = newChannel.Id
	} else if err != nil {
		return err
	} else {
		p.ChannelID = channel.Id
	}

	return nil
}

func (p *Plugin) IsValid(configuration *configuration) error {
	if configuration.TeamChannel == "" {
		return fmt.Errorf("Must set a Team and a Channel.")
	}

	if configuration.AllowedUserIds == "" {
		return fmt.Errorf("Must set at least one User.")
	}

	return nil
}

func (p *Plugin) ServeHTTP(c *plugin.Context, w http.ResponseWriter, r *http.Request) {
	if err := p.checkToken(r); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		p.API.LogError("AWSSNS TOKEN INVALID")
		return
	}
	snsMessageType := r.Header.Get("x-amz-sns-message-type")
	if snsMessageType == "" {
		p.handleAction(w, r)

	} else {
		switch snsMessageType {
		case "SubscriptionConfirmation":
			p.handleSubscriptionConfirmation(r.Body)
			break
		case "Notification":
			p.API.LogDebug("AWSSNS HandleNotification")
			p.handleNotification(r.Body)
			break

		case "UnsubscribeConfirmation":
			p.handleUnsubscribeConfirmation(r.Body)
			break
		default:
			break
		}
	}

	return

}

func (p *Plugin) checkToken(r *http.Request) error {
	token := r.URL.Query().Get("token")
	if token == "" || strings.Compare(token, p.configuration.Token) != 0 {
		return fmt.Errorf("Invalid or missing token")
	}
	return nil
}

func (p *Plugin) handleSubscriptionConfirmation(body io.Reader) {
	var subscribe SubscribeInput
	if err := json.NewDecoder(body).Decode(&subscribe); err != nil {
		return
	}

	p.sendSubscribeConfirmationMessage(subscribe.Message, subscribe.SubscribeURL)
	return
}

func (p *Plugin) handleNotification(body io.Reader) {
	var notification SNSNotification
	if err := json.NewDecoder(body).Decode(&notification); err != nil {
		p.API.LogDebug("AWSSNS HandleNotification Decode Error", "err=", err.Error())
		return
	}

	var messageNotification SNSMessageNotification
	if err := json.Unmarshal([]byte(notification.Message), &messageNotification); err != nil {
		p.API.LogDebug("AWSSNS HandleNotification Decode Error on message notification", "err=", err.Error())
		return
	}

	p.API.LogDebug("AWSSNS HandleNotification", "MESSAGE", notification.Subject)
	var fields []*model.SlackAttachmentField
	fields = addFields(fields, "AlarmName", messageNotification.AlarmName, true)
	fields = addFields(fields, "AlarmDescription", messageNotification.AlarmDescription, true)
	fields = addFields(fields, "AWS Account", messageNotification.AWSAccountID, true)
	fields = addFields(fields, "Region", messageNotification.Region, true)
	fields = addFields(fields, "New State", messageNotification.NewStateValue, true)
	fields = addFields(fields, "Old State", messageNotification.OldStateValue, true)
	fields = addFields(fields, "New State Reason", messageNotification.NewStateReason, false)
	fields = addFields(fields, "MetricName", messageNotification.Trigger.MetricName, true)
	fields = addFields(fields, "Namespace", messageNotification.Trigger.Namespace, true)
	fields = addFields(fields, "StatisticType", messageNotification.Trigger.StatisticType, true)
	fields = addFields(fields, "Statistic", messageNotification.Trigger.Statistic, true)
	fields = addFields(fields, "Period", strconv.Itoa(messageNotification.Trigger.Period), true)
	fields = addFields(fields, "EvaluationPeriods", strconv.Itoa(messageNotification.Trigger.EvaluationPeriods), true)
	fields = addFields(fields, "ComparisonOperator", messageNotification.Trigger.ComparisonOperator, true)
	fields = addFields(fields, "Threshold", fmt.Sprintf("%f", messageNotification.Trigger.Threshold), true)

	var dimensions []string
	for _, dimension := range messageNotification.Trigger.Dimensions {
		dimensions = append(dimensions, fmt.Sprintf("%s: %s", dimension.Name, dimension.Value))
	}
	fields = addFields(fields, "Dimensions", strings.Join(dimensions, "\n"), false)

	msgColor := "#008000"
	if messageNotification.NewStateValue == "ALARM" {
		msgColor = "#FF0000"
	} else if messageNotification.NewStateValue == "INSUFFICIENT" {
		msgColor = "#FFFF00"
	}

	attachment := &model.SlackAttachment{
		Title:  notification.Subject,
		Fields: fields,
		Color:  msgColor,
	}

	post := &model.Post{
		ChannelId: p.ChannelID,
		UserId:    p.BotUserID,
		Props: map[string]interface{}{
			"from_webhook":      "true",
			"override_username": SNS_USERNAME,
			"override_icon_url": SNS_ICON_URL,
		},
	}

	model.ParseSlackAttachment(post, []*model.SlackAttachment{attachment})
	if _, appErr := p.API.CreatePost(post); appErr != nil {
		return
	}
	return
}

func (p *Plugin) handleUnsubscribeConfirmation(body io.Reader) {
	// var subscribe SubscribeInput
	// if err := json.NewDecoder(body).Decode(&subscribe); err != nil {
	// 	return
	// }
	return
}

func (p *Plugin) sendSubscribeConfirmationMessage(message string, subscriptionURL string) {
	config := p.API.GetConfig()
	siteURLPort := *config.ServiceSettings.SiteURL
	action1 := &model.PostAction{
		Name: "Confirm Subscription",
		Type: model.POST_ACTION_TYPE_BUTTON,
		Integration: &model.PostActionIntegration{
			Context: map[string]interface{}{
				"action":           "confirm",
				"subscription_url": subscriptionURL,
			},
			URL: fmt.Sprintf("%v/plugins/%v/confirm?token=%v", siteURLPort, manifest.Id, p.configuration.Token),
		},
	}

	actionMsg := strings.Split(message, ".")
	sa1 := &model.SlackAttachment{
		Text: actionMsg[0],
		Actions: []*model.PostAction{
			action1,
		},
	}
	attachments := make([]*model.SlackAttachment, 0)
	attachments = append(attachments, sa1)

	spinPost := &model.Post{
		Message:   "",
		ChannelId: p.ChannelID,
		UserId:    p.BotUserID,
		Props: model.StringInterface{
			"attachments":       attachments,
			"override_username": SNS_USERNAME,
			"override_icon_url": SNS_ICON_URL,
			"from_webhook":      "true",
		},
	}

	if _, err := p.API.CreatePost(spinPost); err != nil {
		p.API.LogError(
			"We could not create subscription post",
			"user_id", p.BotUserID,
			"err", err.Error(),
		)
	}
	p.API.LogDebug(
		"Posted new subscription",
		"user_id", p.BotUserID,
		"subscriptionURL", subscriptionURL,
	)

}

func (p *Plugin) handleAction(w http.ResponseWriter, r *http.Request) {
	var action *Action
	err := json.NewDecoder(r.Body).Decode(&action)
	if err != nil || action == nil {
		encodeEphermalMessage(w, "SNS BOT Error: We could not decode the action")
		return
	}

	if err := p.checkAllowedUsers(action.UserID); err != nil {
		encodeEphermalMessage(w, err.Error())
		return
	}

	switch r.URL.Path {
	case "/confirm":
		_, err := http.Get(action.Context.SubscriptionURL)
		if err != nil {
			encodeEphermalMessage(w, err.Error())
			return
		}

		updatePost := &model.Post{}
		updateAttachment := &model.SlackAttachment{}
		actionPost, errPost := p.API.GetPost(action.PostID)
		if errPost != nil {
			p.API.LogError("AWSSNS Update Post Error", "err=", errPost.Error())
		} else {
			for _, attachment := range actionPost.Attachments() {
				if attachment.Text != "" {
					userName, errUser := p.API.GetUser(action.UserID)
					if errUser != nil {
						updateAttachment.Text = fmt.Sprintf("%s\n**Subscription Confirmed.**", attachment.Text)
					}
					updateAttachment.Text = fmt.Sprintf("%s\n**Subscription Confirmed by %s**", attachment.Text, userName.Username)
				}
			}
			retainedProps := []string{"override_username", "override_icon_url"}
			updatePost.AddProp("from_webhook", "true")

			for _, prop := range retainedProps {
				if value, ok := actionPost.Props[prop]; ok {
					updatePost.AddProp(prop, value)
				}
			}

			model.ParseSlackAttachment(updatePost, []*model.SlackAttachment{updateAttachment})
			updatePost.Id = actionPost.Id
			updatePost.ChannelId = actionPost.ChannelId
			updatePost.UserId = actionPost.UserId
			if _, err := p.API.UpdatePost(updatePost); err != nil {
				encodeEphermalMessage(w, "Subscription Confirmed.")
				return
			}

			encodeEphermalMessage(w, "Subscription Confirmed.")
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
}

func (p *Plugin) checkAllowedUsers(userID string) error {

	if userID == "" {
		return fmt.Errorf("Need a user id")
	}

	hasPremissions := false
	AllowedUserIds := strings.Split(p.configuration.AllowedUserIds, ",")
	for _, allowedUserID := range AllowedUserIds {
		if allowedUserID == userID {
			hasPremissions = true
			break
		}
	}

	if !hasPremissions {
		return fmt.Errorf("You don't have permissions to use this command. Please talk with your SysAdmin.")
	}

	return nil
}

func encodeEphermalMessage(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	payload := map[string]interface{}{
		"ephemeral_text": message,
	}

	json.NewEncoder(w).Encode(payload)
}

func addFields(fields []*model.SlackAttachmentField, title, msg string, short bool) []*model.SlackAttachmentField {
	return append(fields, &model.SlackAttachmentField{
		Title: title,
		Value: msg,
		Short: model.SlackCompatibleBool(short),
	})
}
