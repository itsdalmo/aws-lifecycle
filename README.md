## aws-lifecycle

[![Build Status](https://travis-ci.com/telia-oss/aws-lifecycle.svg?branch=master)](https://travis-ci.com/telia-oss/aws-lifecycle)

A small daemon for gracefully shutting down instances on autoscaling actions. Inspired by [lifecycled](https://github.com/buildkite/lifecycled).

## Usage

The daemon needs to be running on each individual instance of your autoscaling group, and the instance profile needs to be
granted privileges that allows the daemon to manage the SQS queue where the lifecycle events are delivered. See the terraform
code for an example of a complete setup.
