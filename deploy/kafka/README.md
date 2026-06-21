# Kafka event backbone — MSK IaC (REVIEWED ARTIFACT, NOT APPLIED)

> ## ⚠️ GATED STEP — DO NOT APPLY WITHOUT OPERATOR APPROVAL
> Running `terraform apply` here **provisions a real, billed AWS MSK cluster**
> (~**$0.5–0.7k/mo**). This directory is a *reviewed artifact + runbook*. The
> cluster is **not** provisioned by CI or by `deploy/deploy.sh`. Provisioning is
> a deliberate human step that needs production AWS credentials and an explicit
> go. Plan freely; apply only on an operator's say-so.

This is the Infrastructure-as-Code for the Project Jarvis event backbone: an
**AWS MSK (Managed Streaming for Kafka) Provisioned** cluster plus the topic
definitions the platform's event taps / shadow consumers will use.

## What's here

| File | Purpose |
|---|---|
| `main.tf` | MSK Provisioned cluster (3 brokers / 3 AZs), customer-managed KMS key, dedicated security group, broker configuration, CloudWatch logging, Prometheus open monitoring. |
| `variables.tf` | Inputs. **`vpc_id`, `subnet_ids`, `ecs_task_sg_id` have no defaults — the operator MUST fill them in** from the live prod network. |
| `outputs.tf` | Bootstrap broker strings (incl. the SASL/IAM one), KMS ARN, SG id. |
| `topics.tf` | Topic *spec* as Terraform locals + a materialized `topics.json`. Topics are **not** created by Terraform — see "Topic management" below. |
| `topics.json` | Canonical topic spec (source of truth, consumed by the script). |
| `create_topics.sh` | SASL/IAM admin script that creates/reconciles the topics from **inside the VPC**. |

## Target environment (hard facts)

- Region **us-west-2**, account **146361001621**, AWS profile **jamesventure**.
- Lives alongside ECS cluster **apex-cluster** / service **ignite-service** /
  task family **ignite-upside-down**, container **ignite-server**.
- **Never** point this at us-east-1 — that is a separate, non-prod environment
  with a different database.

## Cluster shape (what `main.tf` declares)

- **3 brokers** across **3 AZs**, instance type **`kafka.m7g.large`**, **gp3 EBS, 100 GiB/broker**.
- Durability (broker config + per-topic RF):
  `default.replication.factor=3`, `min.insync.replicas=2`,
  `unclean.leader.election.enable=false`,
  `offsets.topic.replication.factor=3`,
  `transaction.state.log.replication.factor=3`.
- **Encryption:** in-transit **TLS enforced** (`client_broker=TLS`, no PLAINTEXT);
  at-rest with a **customer-managed KMS key** (defined here, rotation on).
- **Auth:** **IAM (SASL/IAM) only**. SCRAM, mTLS, and unauthenticated access are off.
  Clients connect on **port 9098**.
- **Networking:** private subnets; dedicated MSK SG allows **9098 inbound ONLY
  from the ECS task SG**; intra-cluster traffic via SG self-rule; egress open.
- **Observability:** Prometheus JMX + node exporters (open monitoring) and
  broker logs → CloudWatch (`/aws/msk/<cluster_name>`, 30-day retention).
- `prevent_destroy = true` on the cluster — disruptive changes require a
  deliberate edit, not an accidental replace.

## Prereqs / discover the inputs

The three networking variables must be filled from the **live prod VPC** (the
one apex-cluster ECS tasks run in). Discover them (read-only) with:

```bash
AWS="--profile jamesventure --region us-west-2"

# The VPC + private subnets + the task SG a running ignite-server task uses:
TASK_ARN=$(aws ecs list-tasks --cluster apex-cluster --service-name ignite-service \
  --query 'taskArns[0]' --output text $AWS)
ENI=$(aws ecs describe-tasks --cluster apex-cluster --tasks "$TASK_ARN" \
  --query "tasks[0].attachments[0].details[?name=='networkInterfaceId'].value | [0]" \
  --output text $AWS)
aws ec2 describe-network-interfaces --network-interface-ids "$ENI" \
  --query 'NetworkInterfaces[0].{VpcId:VpcId,SubnetId:SubnetId,Groups:Groups}' $AWS
```

- `VpcId` → `vpc_id`
- `Groups[].GroupId` (the ignite-server task SG) → `ecs_task_sg_id`
- Pick **3 private subnets, one per AZ** (us-west-2a/b/c) in that VPC → `subnet_ids`
  (the task's own subnet plus its sibling private subnets; confirm they're
  private — no `0.0.0.0/0` → igw route).

Put them in a `terraform.tfvars` (do NOT commit real ids if the repo policy
treats infra ids as sensitive):

```hcl
vpc_id         = "vpc-XXXXXXXX"
subnet_ids     = ["subnet-AAA", "subnet-BBB", "subnet-CCC"]
ecs_task_sg_id = "sg-XXXXXXXX"
```

Also required: Terraform >= 1.5, AWS provider ~> 5.40, and AWS creds for the
`jamesventure` profile with MSK/KMS/EC2/CloudWatch create permissions.

## Apply runbook (the gated step)

```bash
cd upside-down/deploy/kafka

# 1. (recommended) wire an S3 backend first — see the commented block in main.tf.
#    Without it, state lives on one laptop.

terraform init

# 2. Review. ALWAYS read the plan before applying.
terraform plan -var-file=terraform.tfvars

# 3. GATED — only with operator approval. Provisions billed infra; takes
#    ~15–30 min for the brokers to come up.
terraform apply -var-file=terraform.tfvars

# 4. Grab the bootstrap string for the app.
terraform output -raw bootstrap_brokers_sasl_iam
```

### Create the topics (after the cluster is up)

Topics are **not** Terraform-managed (see "Topic management"). Run the admin
script **from inside the VPC** — an ECS exec shell on an ignite-server task, or
a bastion — where port 9098 + IAM auth both work:

```bash
BOOTSTRAP="$(terraform output -raw bootstrap_brokers_sasl_iam)" \
MSK_IAM_JAR=/opt/aws-msk-iam-auth-all.jar \
./create_topics.sh --dry-run    # eyeball first
# then for real:
BOOTSTRAP="..." MSK_IAM_JAR=/opt/aws-msk-iam-auth-all.jar ./create_topics.sh
```

Topics created (spec in `topics.json`):

| Topic | Partitions | cleanup.policy | Retention |
|---|---|---|---|
| `evt.lake.v1` | 12 | delete | 7 d |
| `evt.ingest.v1` | 12 | delete | 7 d |
| `suppression.state` | 6 | **compact** | (compacted) |
| `evt.lake.v1.dlq` | 3 | delete | 14 d |
| `evt.ingest.v1.dlq` | 3 | delete | 14 d |
| `suppression.state.dlq` | 3 | delete | 14 d |

All topics use `replication.factor=3` and `min.insync.replicas=2`.

## Wire `KAFKA_BROKERS` into the ECS task def afterward

The app reads the bootstrap string from an env var (`KAFKA_BROKERS`). The deploy
pipeline carries env vars forward via `PASSTHROUGH_ENV_VARS` in
`deploy/prepare_task_definition.py`. To wire it in:

1. **Add `KAFKA_BROKERS` to `PASSTHROUGH_ENV_VARS`** in
   `deploy/prepare_task_definition.py` (one-line addition to the list) so future
   deploys preserve it.
2. **Set it on the next deploy** (it lands in the task def and is then inherited):
   ```bash
   export KAFKA_BROKERS="$(terraform output -raw bootstrap_brokers_sasl_iam)"
   cd upside-down && bash deploy/deploy.sh
   ```
   (A backend rebuild + deploy is required for the app to pick up Kafka — pushing
   git or applying Terraform alone does nothing to the running task.)
3. The IAM **task role** must be granted `kafka-cluster:Connect` +
   topic/group permissions on this cluster's ARN before the app can authenticate.
   Add that policy to the ignite-server task role (out of scope for this module;
   note it as a follow-up).

## Topic management — why not Terraform

Topic CRUD on MSK Provisioned needs a Kafka admin client that can reach the
private brokers on 9098 **and** sign with SASL/IAM. The only Terraform option is
the community `Mongey/kafka` provider, which (a) requires a live TCP path to the
private brokers from wherever `terraform apply` runs, and (b) has weak SASL/IAM
support. Both make `plan`/`apply` from a laptop fail and couple cluster
provisioning to broker reachability — the wrong layer. So the **cluster** is
Terraform-managed and **topics** are created by `create_topics.sh` from inside
the VPC, with the exact specs captured in `topics.json` (and mirrored as TF
locals in `topics.tf` for review parity).

## Estimated cost

~**$0.5–0.7k/month**: 3 × `kafka.m7g.large` brokers (~$0.45/hr each ≈ $1.35/hr ≈
$985/mo list — partly offset in practice) + 3 × 100 GiB gp3 + minimal data
transfer + CloudWatch. Treat $0.5–0.7k/mo as the steady-state planning figure;
confirm against the AWS pricing calculator for us-west-2 before approving.

## Teardown

`prevent_destroy = true` guards the cluster. To decommission deliberately:
remove that lifecycle flag, `terraform plan`, get operator approval, then
`terraform destroy`. The KMS key has a 30-day deletion window.
