package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	flags "github.com/jessevdk/go-flags"
	"github.com/sirupsen/logrus"
	lifecycle "github.com/telia-oss/aws-lifecycle"
)

// Command structure for the CLI
type Command struct {
	Handler     string `long:"handler" description:"Path to a handler (script) that will be executed on termination notice." required:"true"`
	TopicArn    string `short:"t" long:"sns-topic-arn" description:"The ARN of the SNS topic where lifecycle hooks are delivered." required:"true"`
	InstanceID  string `short:"i" long:"instance-id" description:"The instance ID for which to listen for lifecycle hook events."`
	JSONLogging bool   `short:"j" long:"json" description:"Enable JSON logging."`
	LogFile     string `short:"l" long:"log-file" description:"Write logs to a file."`
	Verbosity   []bool `short:"v" long:"verbosity" description:"Control verbosity of logs."`
}

func main() {
	var (
		cmd    Command
		logger *logrus.Logger
	)

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
	switch len(cmd.Verbosity) {
	case 0:
		logger.SetLevel(logrus.WarnLevel)
	case 1:
		logger.SetLevel(logrus.InfoLevel)
	default:
		logger.SetLevel(logrus.DebugLevel)
	}
	if cmd.LogFile != "" {
		f, err := os.OpenFile(cmd.LogFile, os.O_WRONLY|os.O_CREATE, 0755)
		if err != nil {
			logger.WithError(err).Fatal("could not open log file")
		}
		mw := io.MultiWriter(os.Stdout, f)
		logger.SetOutput(mw)
	}

	// Make sure the handler exists
	cmd.Handler, err = exec.LookPath(cmd.Handler)
	if err != nil {
		logger.WithError(err).Fatal("invalid handler")
	}

	sess, err := session.NewSession()
	if err != nil {
		logger.WithError(err).Fatal("failed to create a new session")
	}

	if cmd.InstanceID == "" {
		logger.Debug("getting instance id from ec2 metadata")
		id, err := ec2metadata.New(sess).GetMetadata("instance-id")
		if err != nil {
			logger.WithError(err).Fatal("failed to get instance id from ec2 metadata")
		}
		cmd.InstanceID = id
	}

	// Capture for interrupt signals in order to shut down gracefully
	signals := make(chan os.Signal)
	defer close(signals)

	signal.Notify(signals,
		syscall.SIGKILL,
		syscall.SIGTERM,
		syscall.SIGINT,
	)
	defer signal.Stop(signals)

	// Use context to unwind go-routines. Cancelling will tell the daemon
	// to wind down and close the message channel, which in turn prompts
	// the message handler to return (and allow the deferred calls to run).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		for s := range signals {
			logger.WithField("instanceId", cmd.InstanceID).Infof("got signal (%s) shutting down...", s)
			cancel()
			break
		}
	}()

	handler := lifecycle.NewHandler(
		cmd.Handler,
		cmd.InstanceID,
		cmd.TopicArn,
		autoscaling.New(sess),
		lifecycle.NewQueue(sess),
		logger,
	)

	if err := handler.Listen(ctx); err != nil {
		logger.WithField("instanceId", cmd.InstanceID).WithError(err).Fatal("failed to start daemon")
	}
}
