# Local development settings shared by the helper scripts.
# These credentials only exist on 127.0.0.1; they are not secrets.

export AWS_ACCESS_KEY_ID=celld
export AWS_SECRET_ACCESS_KEY=celld-local-secret
export AWS_REGION=us-east-1

export S3_ENDPOINT=http://127.0.0.1:8333
export S3_BUCKET=celld
export CELLD_BUCKET=s3://$S3_BUCKET

export WEED_MASTER=127.0.0.1:9333
export WEED_S3=$S3_ENDPOINT
export CELLD_LISTEN=127.0.0.1:8787
export CELLD_INTERNAL_LISTEN=127.0.0.1:8788
