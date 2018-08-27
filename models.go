package lifecycle

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sqs"
)

// Message ...
type Message struct {
	ReceiptHandle string
	Body          string
}

// newMessage
func newMessage(m *sqs.Message) *Message {
	return &Message{
		ReceiptHandle: aws.StringValue(m.ReceiptHandle),
		Body:          aws.StringValue(m.Body),
	}
}
