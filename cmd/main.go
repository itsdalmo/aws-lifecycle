package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/aws/aws-sdk-go/aws/session"
	flags "github.com/jessevdk/go-flags"
	"github.com/sirupsen/logrus"
	lifecycle "github.com/telia-oss/aws-lifecycle"
)

// Command structure for the CLI
type Command struct {
	TopicArn    string `short:"t" long:"topic-arn" description:"The ARN of the SNS topic where lifecycle hooks are delivered." required:"true"`
	InstanceID  string `short:"i" long:"instance-id" description:"The instance ID for which to listen for lifecycle hook events."`
	JSONLogging bool   `short:"j" long:"json" description:"Enable JSON logging."`
	Verbose     bool   `short:"v" long:"verbose" description:"Enable verbose (debug) logging."`
}

func main() {
	var (
		cmd    Command
		logger *logrus.Logger
	)

	// Parse CLI flags
	_, err := flags.Parse(&cmd)
	if err != nil {
		if flagsErr, ok := err.(*flags.Error); ok && flagsErr.Type == flags.ErrHelp {
			os.Exit(0)
		} else {
			os.Exit(1)
		}
	}

	logger = logrus.New()
	if cmd.JSONLogging {
		logger.Formatter = &logrus.JSONFormatter{}
	}
	if cmd.Verbose {
		logger.SetLevel(logrus.DebugLevel)
	}
	log := logger.WithField("instanceId", cmd.InstanceID)

	sess, err := session.NewSession()
	if err != nil {
		log.Fatalf("failed to create a new session: %s", err)
	}

	// Capture for interrupt signals in order to shut down gracefully
	signals := make(chan os.Signal)
	defer close(signals)

	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)

	// Use context to unwind go-routines. Cancelling will tell the daemon
	// to wind down and close the message channel, which in turn prompts
	// the message handler to return (and allow the deferred calls to run).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		for s := range signals {
			log.Infof("got signal (%s) shutting down...", s.String())
			cancel()
			break
		}
	}()

	daemon := lifecycle.NewDaemon(cmd.InstanceID, cmd.TopicArn, log)
	if err := daemon.Start(ctx, lifecycle.NewQueue(sess)); err != nil {
		log.Errorf("failed to start daemon: %s", err)
		return
	}
}
