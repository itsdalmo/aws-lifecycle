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

// Daemon is responsible for handling lifecycle actions.
type Daemon struct {
	handler    string
	instanceID string
	topicArn   string

	autoscalingClient AutoscalingClient
	queue             *Queue
	log               *logrus.Entry
}

// NewDaemon creates a new daemon for handling lifecycle events
func NewDaemon(handler string, instanceID, topicArn string, autoscaling AutoscalingClient, queue *Queue, log *logrus.Entry) *Daemon {
	return &Daemon{
		handler:           handler,
		instanceID:        instanceID,
		topicArn:          topicArn,
		autoscalingClient: autoscaling,
		queue:             queue,
		log:               log,
	}
}

// Start the daemon
func (d *Daemon) Start(ctx context.Context) error {
	queueName := fmt.Sprintf("aws-lifecycle-%s", d.instanceID)

	// Create the SQS queue
	d.log.WithField("queueName", queueName).Info("creating queue")
	if err := d.queue.Create(queueName, d.topicArn); err != nil {
		return fmt.Errorf("failed to create queue: %s", err)
	}
	defer func() {
		d.log.WithField("queueName", queueName).Info("deleting queue")
		if err := d.queue.Delete(); err != nil {
			d.log.WithError(err).Error("failed to delete queue")
		}
	}()

	// Subscribe to the STS topic
	d.log.WithField("topicArn", d.topicArn).Info("subscribing to topic")
	if err := d.queue.Subscribe(d.topicArn); err != nil {
		return fmt.Errorf("failed to subscribe to the sns topic: %s", err)
	}
	defer func() {
		d.log.WithField("topicArn", d.topicArn).Info("unsubscribing from topic")
		if err := d.queue.Unsubscribe(); err != nil {
			d.log.WithError(err).Error("failed to unsubscribe from sns topic")
		}
	}()

	messages := make(chan *Message)

	// Listen for new messages
	go d.listen(ctx, messages)
	d.log.Debug("listening for termination notice")

	// Process termination notices
	for m := range messages {
		log := d.log.WithField("messageId", m.MessageID)
		log.Info("received termination notice")

		log.Info("starting heartbeat")
		ticker := time.NewTicker(10 * time.Second)
		go d.heartbeat(ticker, m, log)

		log.Info("executing handler")
		if err := d.execute(ctx, log); err != nil {
			log.WithError(err).Error("failed to execute handler")
		} else {
			log.Info("handler completed successfully")
		}

		log.Info("completing lifecycle")
		if err := d.complete(m); err != nil {
			log.WithError(err).Error("failed to signal lifecycle")
		}
		log.Info("signaled lifecycle to continue")

		// The listener having stopped will bring us out of the loop
		ticker.Stop()
	}

	return nil
}

// listen polls SQS for new messages and passes termination notices to the given channel
func (d *Daemon) listen(ctx context.Context, messages chan<- *Message) {
	defer close(messages)
	defer d.log.Debug("stopping listener...")

	for {
		select {
		case <-ctx.Done():
			return
		default:
			d.log.Debug("requesting new messages")
			out, err := d.queue.getMessage(ctx)
			if err != nil {
				d.log.WithError(err).Warn("failed to get messages")
			}
			for _, m := range out {
				log := d.log.WithField("messageId", m.MessageID)
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
				messages <- m
				return
			}
		}
	}
}

func (d *Daemon) heartbeat(ticker *time.Ticker, m *Message, log *logrus.Entry) {
	defer d.log.Debug("stopping heartbeat...")
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

func (d *Daemon) complete(m *Message) error {
	_, err := d.autoscalingClient.CompleteLifecycleAction(&autoscaling.CompleteLifecycleActionInput{
		AutoScalingGroupName:  aws.String(m.GroupName),
		LifecycleHookName:     aws.String(m.HookName),
		InstanceId:            aws.String(m.InstanceID),
		LifecycleActionToken:  aws.String(m.ActionToken),
		LifecycleActionResult: aws.String("CONTINUE"),
	})
	return err
}
