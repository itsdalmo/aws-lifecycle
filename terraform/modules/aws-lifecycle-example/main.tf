# -------------------------------------------------------------------------------
# Resources
# -------------------------------------------------------------------------------
data "aws_region" "current" {}

data "aws_caller_identity" "current" {}

resource "aws_s3_bucket" "artifact" {
  bucket = "${var.name_prefix}-artifact-${data.aws_caller_identity.current.account_id}"
  acl    = "private"

  tags = "${merge(var.tags, map("Name", "${var.name_prefix}-artifact-${data.aws_caller_identity.current.account_id}"))}"
}

resource "aws_s3_bucket_object" "artifact" {
  bucket = "${aws_s3_bucket.artifact.id}"
  key    = "aws-lifecycle-linux-amd64"
  source = "${var.aws_lifecycle_path}"
  etag   = "${md5(file("${var.aws_lifecycle_path}"))}"
}

data "template_file" "main" {
  template = "${file("${path.module}/cloud-config.yml")}"

  vars {
    region          = "${data.aws_region.current.name}"
    stack_name      = "${var.name_prefix}-asg"
    log_group_name  = "${aws_cloudwatch_log_group.main.name}"
    lifecycle_topic = "${aws_sns_topic.main.arn}"
    artifact_bucket = "${aws_s3_bucket.artifact.id}"
    artifact_key    = "${aws_s3_bucket_object.artifact.id}"
    artifact_etag   = "${aws_s3_bucket_object.artifact.etag}"
  }
}

module "asg" {
  source  = "telia-oss/asg/aws"
  version = "0.2.0"

  name_prefix       = "${var.name_prefix}"
  user_data         = "${data.template_file.main.rendered}"
  vpc_id            = "${var.vpc_id}"
  subnet_ids        = "${var.subnet_ids}"
  await_signal      = "true"
  pause_time        = "PT5M"
  health_check_type = "EC2"
  instance_policy   = "${data.aws_iam_policy_document.permissions.json}"
  min_size          = "${var.instance_count}"
  instance_type     = "${var.instance_type}"
  instance_ami      = "${var.instance_ami}"
  instance_key      = "${var.key_pair}"
  tags              = "${var.tags}"
}

resource "aws_security_group_rule" "ssh_ingress" {
  count             = "${var.key_pair != "" ? 1 : 0}"
  security_group_id = "${module.asg.security_group_id}"
  type              = "ingress"
  protocol          = "tcp"
  from_port         = 22
  to_port           = 22
  cidr_blocks       = ["0.0.0.0/0"]
}

# Enable SSM agent for debugging purposes using: 
# https://github.com/itsdalmo/ssm-sh
module "ssm-agent-policy" {
  source  = "telia-oss/ssm-agent-policy/aws"
  version = "0.1.0"

  name_prefix = "${var.name_prefix}"
  role        = "${module.asg.role_name}"
}

# Create log group for daemon logs
resource "aws_cloudwatch_log_group" "main" {
  name = "${var.name_prefix}-daemon"
}

data "aws_iam_policy_document" "permissions" {
  statement {
    effect = "Allow"

    resources = [
      "${aws_cloudwatch_log_group.main.arn}",
    ]

    actions = [
      "logs:CreateLogStream",
      "logs:CreateLogGroup",
      "logs:PutLogEvents",
    ]
  }

  statement {
    effect = "Allow"

    resources = ["*"]

    actions = [
      "cloudwatch:PutMetricData",
      "cloudwatch:GetMetricStatistics",
      "cloudwatch:ListMetrics",
      "ec2:DescribeTags",
    ]
  }

  statement {
    effect = "Allow"

    resources = [
      "${aws_sns_topic.main.arn}",
    ]

    actions = [
      "sns:Subscribe",
      "sns:Unsubscribe",
    ]
  }

  statement {
    effect = "Allow"

    resources = ["*"]

    actions = [
      "sqs:*",
    ]
  }

  statement {
    effect = "Allow"

    actions = [
      "s3:*",
    ]

    resources = [
      "${aws_s3_bucket.artifact.arn}/*",
      "${aws_s3_bucket.artifact.arn}",
    ]
  }

  statement {
    effect = "Allow"

    resources = ["*"]

    actions = [
      "autoscaling:DescribeAutoScalingInstances",
      "autoscaling:DescribeLifecycleHooks",
      "autoscaling:RecordLifecycleActionHeartbeat",
      "autoscaling:CompleteLifecycleAction",
    ]
  }
}

# The following is required to run aws-lifecycle:
# 1. SNS topic
# 2. Lifecycle hook
# 3. Execution role for the lifecycle hook (which allows it to send messages to the SNS topic)
resource "aws_sns_topic" "main" {
  name = "${var.name_prefix}-lifecycle"
}

resource "aws_autoscaling_lifecycle_hook" "main" {
  name                    = "${var.name_prefix}-lifecycle"
  autoscaling_group_name  = "${module.asg.id}"
  lifecycle_transition    = "autoscaling:EC2_INSTANCE_TERMINATING"
  default_result          = "CONTINUE"
  heartbeat_timeout       = "300"
  notification_target_arn = "${aws_sns_topic.main.arn}"
  role_arn                = "${aws_iam_role.lifecycle.arn}"
}

resource "aws_iam_role" "lifecycle" {
  name               = "${var.name_prefix}-lifecycle-role"
  assume_role_policy = "${data.aws_iam_policy_document.asg_assume.json}"
}

resource "aws_iam_role_policy" "lifecycle" {
  name   = "${var.name_prefix}-lifecycle-permissions"
  role   = "${aws_iam_role.lifecycle.id}"
  policy = "${data.aws_iam_policy_document.asg_permissions.json}"
}

data "aws_iam_policy_document" "asg_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["autoscaling.amazonaws.com"]
    }
  }
}

data "aws_iam_policy_document" "asg_permissions" {
  statement {
    effect = "Allow"

    resources = [
      "${aws_sns_topic.main.arn}",
    ]

    actions = [
      "sns:Publish",
    ]
  }
}
