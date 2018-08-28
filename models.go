package lifecycle

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sqs"
)

// Message ...
type Message struct {
	Notification
	ReceiptHandle string
}

// Envelope ...
type Envelope struct {
	Type    string    `json:"Type"`
	Subject string    `json:"Subject"`
	Time    time.Time `json:"Time"`
	Message string    `json:"Message"`
}

// Notification ...
type Notification struct {
	Time        time.Time `json:"Time"`
	GroupName   string    `json:"AutoScalingGroupName"`
	InstanceID  string    `json:"EC2InstanceId"`
	ActionToken string    `json:"LifecycleActionToken"`
	Transition  string    `json:"LifecycleTransition"`
	HookName    string    `json:"LifecycleHookName"`
}

// newMessage
func newMessage(m *sqs.Message) (*Message, error) {
	var (
		envelope     Envelope
		notification Notification
	)

	if err := json.Unmarshal([]byte(*m.Body), &envelope); err != nil {
		return nil, fmt.Errorf("failed to unmarshal envelope: %s", err)
	}

	if err := json.Unmarshal([]byte(envelope.Message), &notification); err != nil {
		return nil, fmt.Errorf("failed to unmarshal lifecycle notification: %s", err)
	}

	return &Message{
		ReceiptHandle: aws.StringValue(m.ReceiptHandle),
		Notification:  notification,
	}, nil
}
