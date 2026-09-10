# Namecheap Dynamic DNS — one-time setup (Phase 7.4)

`git.cmposer.cc` is kept pointed at the instance's public IP by the `gitd ddns`
subcommand (a `gitd-ddns` container timer running every 6h, per
`gitd.yaml` `ddns.interval`). This is a one-time, manual setup that must
happen before the first `cloudformation/deploy.sh` run.

## 1. Create the Dynamic DNS host on Namecheap

1. Log in to your Namecheap account and open **Domain List → `cmposer.cc` →
   Advanced DNS**.
2. In the **Dynamic DNS** section, set the host to **`git`**.
3. Set a password (any strong value; it is the update-token, not a
   credential of yours).
4. Save. Namecheap now serves `git.cmposer.cc` through
   `dynamicdns.park-your-domain.com`.

> The host must be exactly `git` (matches `gitd.yaml` `ddns.host`); the domain
> is `cmposer.cc` (`ddns.domain`). Namecheap DDNS records expire if not
> refreshed (~30d), which is why the `gitd-ddns` timer refreshes every 6h.

## 2. Store the password in SSM

Push the Dynamic DNS password to SSM as a SecureString so the host can read
it at boot (the boot script writes it to `/etc/gitd/ddns-password`,
root:root 0600, per R6-Q5/R7-Q3):

```sh
aws ssm put-parameter \
  --region us-east-2 \
  --name /gitd/ddns/password \
  --type SecureString \
  --value 'THE_DYNAMIC_DNS_PASSWORD'
```

The instance role already grants `ssm:GetParameter` on `/gitd/*`, so no extra
IAM is needed. The value is never placed in the CloudFormation template
(secrets stay client-side, R3-Q3).

## 3. Verify

After deploy, confirm the record resolves to the instance's public IP:

```sh
host git.cmposer.cc        # -> should return the public IP (Outputs.GitdPublicIp)
```

and that the `gitd-ddns` unit can refresh it:

```sh
systemctl start gitd-ddns.service   # on the host via SSM
systemctl status gitd-ddns.service
```

The `gitd ddns` subcommand hits
`https://dynamicdns.park-your-domain.com/update?host=git&domain=cmposer.cc&password=...`
with the `ip` parameter omitted (Namecheap uses the requester IP = the
instance's auto-assigned public IP).
A "Good <ip>" reply is success; anything else fails loudly (fail-fast).
