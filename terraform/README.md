## aws-lifecycle deployment

Quick way of bootstrapping an autoscaling group with a termination lifecycle hook enabled
and `aws-lifecycle` running on the instance, using terraform. It creates the the following:

- Autoscaling group running Amazon linux 2.
- topic and lifecycle hook which sends termination notices to the topic.
- Log group where each instance will send their `aws-lifecycle` logs.
- S3 bucket for the artfiact (uploaded from your computer).
- An instance profile with permissions for:
  - SSM agent: so that [`ssm-sh`](https://github.com/itsdalmo/ssm-sh) can be used for debugging.
  - Logging to the Cloudwatch log group.
  - Downloading the artifact from the S3 bucket.

### Usage

Compile `aws-lifecycle` for linux with `make linux`.

Have terraform installed, configure [example.tf](./example.tf) and run the following:

```bash
terraform init
terraform apply
```

Terraform state will be stored locally unless you add a remote backend to `example.tf`,
and when you are done testing you can tear everything down with `terraform destroy`.
