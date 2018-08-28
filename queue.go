package lifecycle

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sns"
	"github.com/aws/aws-sdk-go/service/sns/snsiface"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/aws/aws-sdk-go/service/sqs/sqsiface"
)

// SQSClient for testing purposes.
//go:generate mockgen -destination=mocks/mock_sqs_client.go -package=mocks github.com/telia-oss/grawsful SMClient
type SQSClient sqsiface.SQSAPI

// SNSClient for testing purposes.
//go:generate mockgen -destination=mocks/mock_sns_client.go -package=mocks github.com/telia-oss/grawsful SMClient
type SNSClient snsiface.SNSAPI

const queuePolicyDocument = `
{
  "Version":"2012-10-17",
  "Statement":[
    {
      "Effect":"Allow",
      "Principal":"*",
      "Action":"sqs:SendMessage",
      "Resource":"*",
      "Condition":{
        "ArnEquals":{
          "aws:SourceArn":"%s"
        }
      }
    }
  ]
}
`

// Queue manages API calls to handle the SQS queue and SNS subscription.
type Queue struct {
	URL             string
	Arn             string
	subscriptionArn string

	sqsClient SQSClient
	snsClient SNSClient
}

// NewQueue ...
func NewQueue(sess *session.Session) *Queue {
	return &Queue{
		sqsClient: sqs.New(sess),
		snsClient: sns.New(sess),
	}
}

// Create the SQS queue.
func (q *Queue) Create(name, topicArn string) error {
	out, err := q.sqsClient.CreateQueue(&sqs.CreateQueueInput{
		QueueName: aws.String(name),
		Attributes: map[string]*string{
			"ReceiveMessageWaitTimeSeconds": aws.String("20"),
			"Policy":                        aws.String(fmt.Sprintf(queuePolicyDocument, topicArn)),
		},
	})
	if err != nil {
		// CreateQueue is idempotent if the name and attributes match an existing queue
		// (i.e. we cannot not ignore sqs.ErrCodeQueueNameExists)
		return err
	}
	q.URL = aws.StringValue(out.QueueUrl)
	return nil
}

// getArn for the SQS queue.
func (q *Queue) getArn() (string, error) {
	if q.Arn == "" {
		out, err := q.sqsClient.GetQueueAttributes(&sqs.GetQueueAttributesInput{
			QueueUrl:       aws.String(q.URL),
			AttributeNames: aws.StringSlice([]string{"QueueArn"}),
		})
		if err != nil {
			return "", fmt.Errorf("failed to get queue arn: %s", err)
		}
		arn, ok := out.Attributes["QueueArn"]
		if !ok {
			return "", err
		}
		q.Arn = aws.StringValue(arn)
	}
	return q.Arn, nil
}

// Subscribe the SQS queue to a SNS topic.
func (q *Queue) Subscribe(topicArn string) error {
	arn, err := q.getArn()
	if err != nil {
		return err
	}
	out, err := q.snsClient.Subscribe(&sns.SubscribeInput{
		TopicArn: aws.String(topicArn),
		Protocol: aws.String("sqs"),
		Endpoint: aws.String(arn),
	})
	if err != nil {
		return err
	}
	q.subscriptionArn = aws.StringValue(out.SubscriptionArn)
	return nil
}

// getMessage long polls a message from the queue.
func (q *Queue) getMessage(ctx context.Context) ([]*Message, error) {
	var messages []*Message

	out, err := q.sqsClient.ReceiveMessageWithContext(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(q.URL),
		MaxNumberOfMessages: aws.Int64(1),
		VisibilityTimeout:   aws.Int64(0),
		WaitTimeSeconds:     aws.Int64(20),
	})
	if err != nil {
		if e, ok := err.(awserr.Error); ok && e.Code() == request.CanceledErrorCode {
			return messages, nil
		}
		return nil, err
	}
	for _, m := range out.Messages {
		messages = append(messages, newMessage(m))
	}
	return messages, nil
}

// deleteMessage from the SQS queue (given a receipt).
func (q *Queue) deleteMessage(receiptHandle string) error {
	_, err := q.sqsClient.DeleteMessageWithContext(context.TODO(), &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(q.URL),
		ReceiptHandle: aws.String(receiptHandle),
	})
	if err != nil {
		return err
	}
	return nil
}

// Unsubscribe the SQS queue from any topic it was subscribed to.
func (q *Queue) Unsubscribe() error {
	_, err := q.snsClient.Unsubscribe(&sns.UnsubscribeInput{
		SubscriptionArn: aws.String(q.subscriptionArn),
	})
	if err != nil {
		return err
	}
	return nil
}

// Delete the SQS queue.
func (q *Queue) Delete() error {
	_, err := q.sqsClient.DeleteQueue(&sqs.DeleteQueueInput{
		QueueUrl: aws.String(q.URL),
	})
	if err != nil {
		// Ignore error if queue does not exist (which is what we want)
		if e, ok := err.(awserr.Error); !ok || e.Code() != sqs.ErrCodeQueueDoesNotExist {
			return err
		}
	}
	return nil
}
