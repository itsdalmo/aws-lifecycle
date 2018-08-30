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

// Handler ...
type Handler struct {
	handler    string
	instanceID string
	topicArn   string

	autoscalingClient AutoscalingClient
	queue             *Queue
	logger            *logrus.Logger
}

// NewHandler ...
func NewHandler(
	handler string,
	instanceID,
	topicArn string,
	autoscaling AutoscalingClient,
	queue *Queue,
	logger *logrus.Logger,
) *Handler {
	return &Handler{
		handler:           handler,
		instanceID:        instanceID,
		topicArn:          topicArn,
		autoscalingClient: autoscaling,
		queue:             queue,
		logger:            logger,
	}
}

// Listen ...
func (d *Handler) Listen(ctx context.Context) error {
	log := d.logger.WithField("instanceId", d.instanceID)

	// Create the SQS queue
	queueName := fmt.Sprintf("aws-lifecycle-%s", d.instanceID)
	log.WithField("queueName", queueName).Info("creating queue")
	if err := d.queue.Create(queueName, d.topicArn); err != nil {
		return fmt.Errorf("failed to create queue: %s", err)
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
		return fmt.Errorf("failed to subscribe to the sns topic: %s", err)
	}
	defer func() {
		log.WithField("topicArn", d.topicArn).Info("unsubscribing from topic")
		if err := d.queue.Unsubscribe(); err != nil {
			log.WithField("topicArn", d.topicArn).WithError(err).Error("failed to unsubscribe from sns topic")
		}
	}()

	messages := make(chan *Message)

	// Listen for new messages
	go d.poll(ctx, messages, log)
	log.Info("listening for termination notices")

	// Process termination notices
	for m := range messages {
		log := log.WithField("messageId", m.MessageID)
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

func (d *Handler) poll(ctx context.Context, messages chan<- *Message, log *logrus.Entry) {
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
				messages <- m
				return
			}
		}
	}
}

func (d *Handler) heartbeat(ticker *time.Ticker, m *Message, log *logrus.Entry) {
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

func (d *Handler) execute(ctx context.Context, log *logrus.Entry) error {
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

func (d *Handler) complete(m *Message) error {
	_, err := d.autoscalingClient.CompleteLifecycleAction(&autoscaling.CompleteLifecycleActionInput{
		AutoScalingGroupName:  aws.String(m.GroupName),
		LifecycleHookName:     aws.String(m.HookName),
		InstanceId:            aws.String(m.InstanceID),
		LifecycleActionToken:  aws.String(m.ActionToken),
		LifecycleActionResult: aws.String("CONTINUE"),
	})
	return err
}
