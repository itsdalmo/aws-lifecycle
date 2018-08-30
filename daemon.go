package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/sirupsen/logrus"
)

// AutoscalingClient for testing purposes.
//go:generate mockgen -destination=mocks/mock_autoscaling_client.go -package=mocks github.com/telia-oss/aws-lifecycle AutoscalingClient
type AutoscalingClient autoscalingiface.AutoScalingAPI

// Daemon ...
type Daemon struct {
	handler    string
	instanceID string
	topicArn   string

	autoscalingClient AutoscalingClient
	queue             *Queue
	logger            *logrus.Logger
}

// NewDaemon ...
func NewDaemon(
	handler string,
	instanceID,
	topicArn string,
	autoscaling AutoscalingClient,
	queue *Queue,
	logger *logrus.Logger,
) *Daemon {
	return &Daemon{
		handler:           handler,
		instanceID:        instanceID,
		topicArn:          topicArn,
		autoscalingClient: autoscaling,
		queue:             queue,
		logger:            logger,
	}
}

// Start runs the daemon and only returns when a termination notice is received, or
// the process is interrupted by a signal. In the first case, it returns a function
// which can be used to signal that the lifecycle hook should proceed.
func (d *Daemon) Start(ctx context.Context) (completeFunc func() error, err error) {
	log := d.logger.WithField("instanceId", d.instanceID)

	// Create the SQS queue
	queueName := fmt.Sprintf("aws-lifecycle-%s", d.instanceID)
	log.WithField("queueName", queueName).Info("creating queue")
	if err := d.queue.Create(queueName, d.topicArn); err != nil {
		return completeFunc, fmt.Errorf("failed to create queue: %s", err)
	}
	defer func() {
		log.WithField("queueName", queueName).Info("deleting queue")
		if err := d.queue.Delete(); err != nil {
			log.WithField("queueName", queueName).WithError(err).Error("failed to delete queue")
		}
	}()

	// Subscribe to the STS topic
	log.WithField("topicArn", d.topicArn).Info("subscribing to topic")
	if err := d.queue.Subscribe(d.topicArn); err != nil {
		return completeFunc, fmt.Errorf("failed to subscribe to the sns topic: %s", err)
	}
	defer func() {
		log.WithField("topicArn", d.topicArn).Info("unsubscribing from topic")
		if err := d.queue.Unsubscribe(); err != nil {
			log.WithField("topicArn", d.topicArn).WithError(err).Error("failed to unsubscribe from sns topic")
		}
	}()

	// Listen for new messages
	messages := make(chan *Message)
	go d.poll(ctx, messages, log)
	log.Info("listening for termination notices")

	// Process termination notices
	for m := range messages {
		log := log.WithField("messageId", m.MessageID)

		ticker := time.NewTicker(10 * time.Second)
		go d.heartbeat(ticker, m, log)

		if err := d.execute(ctx, log); err != nil {
			log.WithError(err).Error("failed to execute handler")
		} else {
			log.Info("handler completed successfully")
		}

		completeFunc = d.complete(m, ticker, log)
	}

	return completeFunc, nil
}

func (d *Daemon) poll(ctx context.Context, messages chan<- *Message, log *logrus.Entry) {
	defer close(messages)
	defer log.Debug("stopped polling...")

	for {
		select {
		case <-ctx.Done():
			return
		default:
			log.Debug("requesting new messages")
			out, err := d.queue.getMessage(ctx)
			if err != nil {
				log.WithError(err).Warn("failed to get messages")
			}
			for _, m := range out {
				log := log.WithField("messageId", m.MessageID)
				if err := d.queue.deleteMessage(ctx, m.ReceiptHandle); err != nil {
					log.WithError(err).Warn("failed to delete message")
				}
				if m.InstanceID != d.instanceID {
					log.Debugf("ignoring notice: target instance id does not match: %s", m.InstanceID)
					continue
				}
				if m.Transition != "autoscaling:EC2_INSTANCE_TERMINATING" {
					log.Debugf("ignoring notice: not a termination notice: %s", m.Transition)
					continue
				}
				log.Info("received termination notice")
				messages <- m
				return
			}
		}
	}
}

func (d *Daemon) heartbeat(ticker *time.Ticker, m *Message, log *logrus.Entry) {
	log.Info("starting heartbeat")
	defer log.Debug("stopping heartbeat...")
	for range ticker.C {
		log.Debug("sending heartbeat")
		_, err := d.autoscalingClient.RecordLifecycleActionHeartbeat(&autoscaling.RecordLifecycleActionHeartbeatInput{
			AutoScalingGroupName: aws.String(m.GroupName),
			LifecycleHookName:    aws.String(m.HookName),
			InstanceId:           aws.String(m.InstanceID),
			LifecycleActionToken: aws.String(m.ActionToken),
		})
		if err != nil {
			log.WithError(err).Warn("failed to send heartbeat")
		}
	}
}

func (d *Daemon) execute(ctx context.Context, log *logrus.Entry) error {
	log.Info("executing handler")
	cmd := exec.Command(d.handler)
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	finished := make(chan error)
	go func() {
		defer close(finished)
		finished <- cmd.Wait()
	}()

	for {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				if err := cmd.Process.Signal(os.Interrupt); err != nil {
					log.WithError(err).Error("failed to interrupt execution of handler")
				}
			}
			return errors.New("execution interrupted by cancelled context")
		case err := <-finished:
			return err
		}
	}
}

func (d *Daemon) complete(m *Message, ticker *time.Ticker, log *logrus.Entry) func() error {
	return func() error {
		log.Info("signaling to proceed with lifecycle action")
		defer ticker.Stop()

		_, err := d.autoscalingClient.CompleteLifecycleAction(&autoscaling.CompleteLifecycleActionInput{
			AutoScalingGroupName:  aws.String(m.GroupName),
			LifecycleHookName:     aws.String(m.HookName),
			InstanceId:            aws.String(m.InstanceID),
			LifecycleActionToken:  aws.String(m.ActionToken),
			LifecycleActionResult: aws.String("CONTINUE"),
		})
		return err
	}
}
