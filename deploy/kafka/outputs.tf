###############################################################################
# outputs.tf — what the operator needs to wire the cluster into the app.
###############################################################################

output "cluster_arn" {
  description = "ARN of the MSK cluster."
  value       = aws_msk_cluster.apex_events.arn
}

output "cluster_name" {
  description = "Name of the MSK cluster."
  value       = aws_msk_cluster.apex_events.cluster_name
}

# THE one the app needs. SASL/IAM bootstrap string -> set as KAFKA_BROKERS in
# the ECS task def (see README "Wire KAFKA_BROKERS into the ECS task def").
output "bootstrap_brokers_sasl_iam" {
  description = "Comma-separated SASL/IAM bootstrap broker string (port 9098). Set this as KAFKA_BROKERS in the ignite-server ECS task def."
  value       = aws_msk_cluster.apex_events.bootstrap_brokers_sasl_iam
}

output "bootstrap_brokers_tls" {
  description = "TLS bootstrap broker string (port 9094). Informational — the app authenticates with IAM, use bootstrap_brokers_sasl_iam."
  value       = aws_msk_cluster.apex_events.bootstrap_brokers_tls
}

output "kms_key_arn" {
  description = "ARN of the customer-managed KMS key used for at-rest encryption."
  value       = aws_kms_key.msk.arn
}

output "msk_security_group_id" {
  description = "ID of the MSK security group (9098 inbound from the ECS task SG only)."
  value       = aws_security_group.msk.id
}

output "cloudwatch_log_group" {
  description = "CloudWatch log group receiving broker logs."
  value       = aws_cloudwatch_log_group.msk.name
}
