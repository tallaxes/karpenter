export CLUSTER_NAME="${USER}-karpenter-demo-v4"
export AWS_DEFAULT_REGION="us-west-2"
export AWS_ACCOUNT_ID="$(aws sts get-caller-identity --query Account --output text)"
