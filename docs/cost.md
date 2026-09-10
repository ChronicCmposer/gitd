# Monthly Cost Note (Phase 8.3)

git.cmposer.cc runs at approximately **$4.50/month** in `us-east-2`. This is
the standing cost model from the plan (Goal, decision table) and a note for
budgeting; how your bill actually lands depends on the exact instance
utilization and the S3 pennies.

## Breakdown

| Item | Spec | Est. monthly |
|------|------|--------------|
| EC2 instance | `t4g.nano`, on-demand, `us-east-2` | ~$3.20 |
| EBS root volume | 8 GB `gp3`, encrypted (default `aws/ebs` key, R4-Q7) | ~$0.6–0.7 |
| S3 buckets | `git.cmposer.cc` — bundles under `repos/`, dist artifacts + image under `openssh/`/`git/`/`fish/`/`containerd/`/`image/`/`bundles/`; **pennies** at this volume | < $0.25 |
| **Total** | | **~$4.50/mo** |

## Why each line is what it is

- **`t4g.nano` on-demand** is the proven minimal sizing from the reference
  projects (2 vCPU / 512MiB). Everything is tuned to fit that budget: the
  per-container memory caps (sshd 320MiB / serve 128MiB / ddns 64MiB, R8-Q4),
  `pack.threads 1` + `gc.auto 5000` (R8-Q9), and the `gitd-ddns`/`gitd-cert-sync`
  lightweight timers. Letting total resident container footprint exceed
  ~512MiB would be the main way this number moves (a pathological git index-pack
  OOMs rather than starving the host — that is the point of the caps).
- **No Elastic IP.** The instance uses an auto-assigned public IPv4
  (`MapPublicIpOnLaunch: true`); there is no allocation to pay for and no
  detached-IP charge risk. DDNS keeps `git.cmposer.cc` pointed at the current
  address (Option B).
- **S3 is "pennies" at this scale**: a handful of bundle objects/week under
  `repos/`, versioned (noncurrent versions expire in 30d, R12-Q9), plus the
  dist/image artifacts. Even with `AbortIncompleteMultipartUpload 7d` belt-and-
  braces, storage + requests here don't move the total meaningfully.
- **SSM, CloudFormation, Session Manager** are all free-tier/management plane —
  no line item. There are no NAT Gateways or Load Balancers (self-contained
  single-instance architecture).

## Tracking

```sh
# No EIP is used (Option B): the address pool should stay empty.
aws ec2 describe-addresses --region us-east-2 \
  --query 'Addresses' --output table

# S3 bucket size (should stay small / pennies).
aws s3 ls s3://git.cmposer.cc/ --recursive --region us-east-2 | awk '{s+=$3} END {printf "S3 bytes: %.1f MiB\n", s/1048576}'

# The authoritative cost number is in Cost Explorer under
# service:EC2-Other(gp3) + AmazonS3 for us-east-2.
```

Run `docs/verification.md` after any infra change (rebuild, update, CA rekey)
and confirm the public IP + DDNS record are current — that single check
protects the least obvious line item.
