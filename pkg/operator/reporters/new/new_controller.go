package new

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eparis/bugzilla"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	slackgo "github.com/slack-go/slack"
	errorutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog"

	"github.com/openshift-eng/shodan/pkg/cache"
	"github.com/openshift-eng/shodan/pkg/operator/bugutil"
	"github.com/openshift-eng/shodan/pkg/operator/config"
	"github.com/openshift-eng/shodan/pkg/operator/controller"
	"github.com/openshift-eng/shodan/pkg/slack"
)

type NewBugReporter struct {
	controller.ControllerContext
	config        config.OperatorConfig
	components    []string
	takeBlockerID string
	slackGoClient *slackgo.Client

	messagesLock             sync.Mutex
	messagesToWatchAndUpdate []message
}

type message struct {
	createdAt time.Time

	// bugzilla
	ID int

	// slack
	channelID string
	ts        string
}

func NewNewBugReporter(ctx controller.ControllerContext, components, schedule []string, operatorConfig config.OperatorConfig, slackGoClient *slackgo.Client, recorder events.Recorder) factory.Controller {
	c := &NewBugReporter{
		ControllerContext: ctx,
		config:            operatorConfig,
		components:        components,
		takeBlockerID:     fmt.Sprintf("new-bugs-reporter/take-%s", strings.Join(components, "-")),
		slackGoClient:     slackGoClient,
	}

	if err := ctx.SubscribeBlockAction(c.takeBlockerID, c.takeClicked); err != nil {
		klog.Warning(err)
	}

	go c.updateMessages()

	return factory.New().WithSync(c.sync).ResyncSchedule(schedule...).ToController("NewBugReporter", recorder)
}

func (c *NewBugReporter) updateMessages() {
	for {
		func() {
			defer func() {
				if err := recover(); err != nil {
					klog.Error(err)
				}
			}()

			client := c.NewBugzillaClient(context.Background())

			c.messagesLock.Lock()
			defer c.messagesLock.Unlock()

			// filter out old messages, older than a month
			notTooOldMessages := make([]message, 0, len(c.messagesToWatchAndUpdate))
			for _, m := range c.messagesToWatchAndUpdate {
				if m.createdAt.Before(time.Now().Add(-time.Hour * 24 * 30)) {
					continue
				}
				notTooOldMessages = append(notTooOldMessages, m)
			}
			c.messagesToWatchAndUpdate = notTooOldMessages

			// reconcile with bugzilla
			messagesToWatch := make([]message, 0, len(c.messagesToWatchAndUpdate))
			for _, m := range c.messagesToWatchAndUpdate {
				b, _, err := client.GetCachedBug(m.ID, "")
				if err != nil {
					messagesToWatch = append(messagesToWatch, m)
					klog.Errorf("failed to get bug %d: %v", m.ID, err)
					continue
				}

				if b.Status == "NEW" {
					messagesToWatch = append(messagesToWatch, m)
					continue
				}

				text := fmt.Sprintf("%s – assigned to %s", bugutil.FormatBugMessage(*b), b.AssignedTo)
				klog.Infof("Updating message to: %v", text)
				if _, _, _, err := c.slackGoClient.UpdateMessage(
					m.channelID,
					m.ts,
					slackgo.MsgOptionBlocks(
						slackgo.NewSectionBlock(slackgo.NewTextBlockObject("mrkdwn", text, false, false), nil, nil),
					),
				); err != nil {
					klog.Errorf("Failed to update message: %v", err)
				}
			}
			c.messagesToWatchAndUpdate = messagesToWatch

			time.Sleep(time.Hour)
		}()
	}
}

func (c *NewBugReporter) sync(ctx context.Context, syncCtx factory.SyncContext) (err error) {
	client := c.NewBugzillaClient(ctx)
	slackClient := c.SlackClient(ctx)

	stateKey := "new-bug-reporter.state-" + strings.Join(c.components, "-")
	lastID := 0
	if s, err := c.GetPersistentValue(ctx, stateKey); err != nil {
		return err
	} else if s != "" {
		lastID, err = strconv.Atoi(s)
		if err != nil {
			klog.Warningf("Cannot parse state value for %s: %v", stateKey, err)
			lastID = 0 // keep going
		}
	}
	defer func() {
		if persistErr := c.SetPersistentValue(ctx, stateKey, strconv.Itoa(lastID)); persistErr != nil {
			if err == nil {
				err = persistErr
			}
		}
	}()

	newBugs, err := getNewBugs(client, c.components, lastID)
	if err != nil {
		syncCtx.Recorder().Warningf("BuglistFailed", err.Error())
		return err
	}

	c.messagesLock.Lock()
	defer c.messagesLock.Unlock()

	var errs []error
	for _, b := range newBugs {
		if b.ID > lastID {
			lastID = b.ID
		}

		value, _ := json.Marshal(TakeValue{b.ID, b.AssignedTo})
		ch, ts, err := slackClient.PostMessageChannel(
			slackgo.MsgOptionBlocks(
				slackgo.NewSectionBlock(slackgo.NewTextBlockObject("mrkdwn", bugutil.FormatBugMessage(*b), false, false), nil, nil),
				slackgo.NewActionBlock(c.takeBlockerID,
					slackgo.NewButtonBlockElement("btn", string(value), slackgo.NewTextBlockObject("plain_text", "Take this Bug", true, false)).WithStyle(slackgo.StylePrimary),
				),
			),
		)
		if err == nil {
			c.messagesToWatchAndUpdate = append(c.messagesToWatchAndUpdate, message{time.Now(), b.ID, ch, ts})
		}
	}

	return errorutil.NewAggregate(errs)
}

type TakeValue struct {
	ID          int    `json:"id"`
	OldAssignee string `json:"oldAssignee"`
}

func (c *NewBugReporter) takeClicked(ctx context.Context, message *slackgo.Container, user *slackgo.User, action *slackgo.BlockAction) {
	var value TakeValue
	if err := json.Unmarshal([]byte(action.Value), &value); err != nil {
		klog.Warningf("cannot unmarshal value %q: %v", action.Value, err)
		return
	}

	// we only have 3s to respond to Slack, but BZ might take longer. Do the work in a go routine
	client := c.NewBugzillaClient(context.Background())
	slackClient := c.SlackClient(context.Background())
	go func() {
		profile, err := c.slackGoClient.GetUserProfile(user.ID, false)
		if err != nil {
			slackClient.PostMessageChannel(
				slackgo.MsgOptionPostEphemeral(user.ID),
				slackgo.MsgOptionText(fmt.Sprintf("Failed to get user profile of %v: %v", user.ID, err), false),
			)
			klog.Errorf("Failed to get user profile of %v: %v", user.ID, err)
			return
		}
		bzEmail := slack.SlackEmailToBugzilla(&c.config, profile.Email)

		b, _, err := client.GetCachedBug(value.ID, "")
		if err != nil {
			slackClient.PostMessageChannel(
				slackgo.MsgOptionPostEphemeral(user.ID),
				slackgo.MsgOptionText(fmt.Sprintf("Failed to get https://bugzilla.redhat.com/show_bug.cgi?id=%v: %v", value.ID, err), false),
			)
			klog.Errorf("Failed to get bug #%v: %v", value.ID, err)
			return
		}
		if b.Status != "NEW" {
			slackClient.PostMessageChannel(
				slackgo.MsgOptionPostEphemeral(user.ID),
				slackgo.MsgOptionText(fmt.Sprintf("Bug https://bugzilla.redhat.com/show_bug.cgi?id=%v has been moved already to %s", value.ID, b.Status), false),
			)
			klog.Infof("Bug #%v not NEW anymore, but %q", value.ID, b.Status)
			return
		}
		if b.AssignedTo != "" && b.AssignedTo != value.OldAssignee {
			slackClient.PostMessageChannel(
				slackgo.MsgOptionPostEphemeral(user.ID),
				slackgo.MsgOptionText(fmt.Sprintf("Bug https://bugzilla.redhat.com/show_bug.cgi?id=%v has already been assigned to %s", value.ID, value.OldAssignee), false),
			)
			klog.Infof("Bug #%v changed assigned, expected %q, got %q", value.ID, value.OldAssignee, b.AssignedTo)
			return
		}

		if err := client.UpdateBug(value.ID, bugzilla.BugUpdate{Status: "ASSIGNED", AssignedTo: bzEmail}); err != nil {
			slackClient.PostMessageChannel(
				slackgo.MsgOptionPostEphemeral(user.ID),
				slackgo.MsgOptionText(fmt.Sprintf("Failed to assign https://bugzilla.redhat.com/show_bug.cgi?id=%v to %s: %v", value.ID, bzEmail, err), false),
			)
			klog.Errorf("Failed to assign bug #%v to %s: %v", value.ID, bzEmail, err)
			return
		}

		b, _, err = client.GetCachedBug(value.ID, "")
		if err != nil {
			slackClient.MessageChannel(fmt.Sprintf("%s took: %s", bzEmail, bugutil.FormatBugMessage(*b)))
			klog.Errorf("Failed to get updated bug #%d: %v", value.ID, err)
			return
		}

		text := fmt.Sprintf("%s – assigned to %s", bugutil.FormatBugMessage(*b), bzEmail)
		klog.Infof("Updating message to: %v", text)
		if _, _, _, err := c.slackGoClient.UpdateMessage(
			message.ChannelID,
			message.MessageTs,
			slackgo.MsgOptionBlocks(
				slackgo.NewSectionBlock(slackgo.NewTextBlockObject("mrkdwn", text, false, false), nil, nil),
			),
		); err != nil {
			slackClient.MessageChannel(fmt.Sprintf("%s took: %s", bzEmail, bugutil.FormatBugMessage(*b)))
			klog.Errorf("Failed to update message: %v", err)
		}
	}()
}
func Report(ctx context.Context, client cache.BugzillaClient, components []string) (string, error) {
	newBugs, err := getNewBugs(client, components, 0)
	if err != nil {
		return "", err
	}

	lines := []string{"New bugs of the last week (excluding those already in a different state):", ""}
	for i, b := range newBugs {
		lines = append(lines, fmt.Sprintf("> %s", bugutil.FormatBugMessage(*b)))
		if i > 20 {
			lines = append(lines, fmt.Sprintf(" ... and %d more", len(newBugs)-20))
			break
		}
	}

	return strings.Join(lines, "\n"), nil
}

func getNewBugs(client cache.BugzillaClient, components []string, lastID int) ([]*bugzilla.Bug, error) {
	aq := bugzilla.AdvancedQuery{
		Field: "bug_id",
		Op:    "greaterthan",
		Value: strconv.Itoa(lastID),
	}
	if lastID == 0 {
		aq = bugzilla.AdvancedQuery{
			Field: "creation_ts",
			Op:    "greaterthaneq",
			Value: "-168h", // last week
		}
	}

	return client.Search(bugzilla.Query{
		Classification: []string{"Red Hat"},
		Product:        []string{"OpenShift Container Platform"},
		Status:         []string{"NEW"},
		Component:      components,
		Advanced:       []bugzilla.AdvancedQuery{aq},
		IncludeFields: []string{
			"id",
			"assigned_to",
			"status",
			"severity",
			"priority",
			"component",
			"summary",
			"cf_cust_facing",
			"target_release",
			"last_change_time",
			"reporter",
		},
	})
}
