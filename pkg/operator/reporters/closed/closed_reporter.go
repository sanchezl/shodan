package closed

import (
	"context"
	"fmt"
	"strings"

	"github.com/eparis/bugzilla"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"

	"github.com/mfojtik/bugzilla-operator/pkg/operator/bugutil"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/config"
	"github.com/mfojtik/bugzilla-operator/pkg/slack"
)

const bugzillaEndpoint = "https://bugzilla.redhat.com"

type BlockersReporter struct {
	config      config.OperatorConfig
	newBugzillaClient func() bugzilla.Client
	slackClient slack.ChannelClient
}

func NewClosedReporter(operatorConfig config.OperatorConfig, scheduleInformer factory.Informer, newBugzillaClient func() bugzilla.Client, slackClient slack.ChannelClient, recorder events.Recorder) factory.Controller {
	c := &BlockersReporter{
		config:      operatorConfig,
		newBugzillaClient: newBugzillaClient,
		slackClient: slackClient,
	}
	return factory.New().WithSync(c.sync).WithInformers(scheduleInformer).ToController("BlockersReporter", recorder)
}

func (c *BlockersReporter) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	client := c.newBugzillaClient()
	report, err := Report(ctx, client, syncCtx.Recorder(), &c.config)
	if err != nil {
		return err
	}
	if len(report) == 0 {
		return nil
	}

	if err := c.slackClient.MessageChannel(report); err != nil {
		syncCtx.Recorder().Warningf("DeliveryFailed", "Failed to deliver closed bug counts: %v", err)
		return err
	}

	return nil
}

func Report(ctx context.Context, client bugzilla.Client, recorder events.Recorder, config *config.OperatorConfig) (string, error) {
	closedBugs, err := client.BugList(config.Lists.Closed.Name, config.Lists.Closed.SharerID)
	if err != nil {
		recorder.Warningf("BuglistFailed", err.Error())
		return "", err
	}

	resolutionMap := map[string][]bugzilla.Bug{}
	for _, bug := range closedBugs {
		resolutionMap[bug.Resolution] = append(resolutionMap[bug.Resolution], bug)
	}

	message := []string{}
	for resolution, bugs := range resolutionMap {
		ids := []string{}
		for _, b := range bugs {
			ids = append(ids, fmt.Sprintf("<https://bugzilla.redhat.com/show_bug.cgi?id=%d|#%d>", b.ID, b.ID))
		}
		p := "bugs"
		if len(bugs) == 1 {
			p = "bug"
		}
		message = append(message, fmt.Sprintf("> %d %s closed as _%s_ (%s)", len(bugs), p, resolution, strings.Join(ids, ",")))
	}

	if len(closedBugs) == 0 {
		return "", nil
	}

	report := fmt.Sprintf("*%s Closed in the last 24h*:\n%s\n", bugutil.BugCountPlural(len(closedBugs), true), strings.Join(message, "\n"))
	return report, nil
}
