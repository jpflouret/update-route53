# Update AWS Route 53 Docker Container

This docker container periodically updates a route53 A record with an external
IP address. This is usefull if you have a dynamic IP address and want to update
the current IP in an AWS route 53 record.

## Usage

### Docker

```shell
docker run -d \
    --name update-route53 \
    -e AWS_ACCESS_KEY_ID=<your access key id> \
    -e AWS_SECRET_ACCESS_KEY=<your secret access key> \
    -e AWS_DEFAULT_REGION=<your aws region> \
    -e HOSTED_ZONE_ID=<your route53 hoseted zone id> \
    -e DNS_NAME=myhost.domain.com \
    ghcr.io/jpflouret/update-route53:latest
```

### Kubernetes Helm Chart

Add the chart repo:
```shell
helm repo add update-route53 https://jpflouret.github.io/update-route53/
helm repo update
```

Create `my-values.yaml` configuration file with default values:
```shell
helm show values update-route53/update-route53 > my-values.yaml
```

Edit the `my-values.yaml` file and set the values as required:
| Key            | Required? | Description                                                                    | Default                                                    |
| -------------- | --------- | ------------------------------------------------------------------------------ | -----------------------------------------------------------|
| `dnsName`      | Yes       | Host name to update                                                            | `""`                                                       |
| `hostedZoneId` | Yes       | Hosted zone id to update                                                       | `""`                                                       |
| `dnsTTL`       | No        | TTL for the DNS record                                                         | `300`<br>(Default in executable)                           |
| `chechIPURL`   | No        | URL to check the public IP address                                             | `http://checkip.amazonaws.com/`<br>(Default in executable) |
| `sleepPeriod`  | No        | Sleep period between IP address checks                                         | `5m`                                                       |
| `tolerations`  | No        | List of kubernetes node taints that are tolerated by the `update-route53` pods | Empty                                                      |
| `nodeSelector` | No        | List of labels used to select which nodes can run `update-route53` pods        | Empty                                                      |

Recommended tolerations to allow execution in control plane nodes:
```yaml
tolerations:
  - key: node-role.kubernetes.io/control-plane
    operator: Exists
    effect: NoSchedule
  - key: node-role.kubernetes.io/master
    operator: Exists
    effect: NoSchedule
```

#### AWS Credentials
The AWS credentials are supplied to the pod with a kubernetes secret.
The secret should contain the following keys:
- `AWS_ACCESS_KEY_ID`
- `AWS_SECRET_ACCESS_KEY`
- `AWS_DEFAULT_REGION`

You can create the secret using `kubectl`:
```shell
kubectl create secret generic aws-credentials \
  --namespace <namespace> \
  --from-literal=AWS_ACCESS_KEY_ID=<access key id> \
  --from-literal=AWS_SECRET_ACCESS_KEY=<secret access key> \
  --from-literal=AWS_DEFAULT_REGION=<aws region>
```
> [!TIP]
> Add a space in front of the above command to prevent bash from
> storing the command in the history file.

When installing the chart, set `secret.existingSecret` to the name of
the secret created above (`aws-credentials` in this example):
```shell
helm install \
  my-update-route53 \
  update-route53/update-route53 \
  --namespace <namespace> \
  --values my-values.yaml \
  --set=secret.existingSecret=aws-credentials
```

Alternatively, the secret can be created at chart installation time using
the following configuration values:
| Key                      | Required?                               | Description                                                  | Default                                   |
| ------------------------ | ----------------------------------------| ------------------------------------------------------------ | ------------------------------------------|
| `secret.create`          | No                                      | Set to `true` to create a secret as part of the Helm release | `false`                                   |
| `secret.accessKeyId`     | Yes if `secret.create` is set to `true` | AWS Access Key ID to use when creating the secret            | `""`                                      |
| `secret.secretAccessKey` | Yes if `secret.create` is set to `true` | AWS Secret Access Key to use when creating the secret        | `""`                                      |
| `secret.awsRegion`       | No                                      | AWS Region to use when creating the secret                   | `"us-west-2"`<br>(Defaults in executable) |

#### Metrics
The pod exposes prometheus metrics on port `8080` on the `/metrics` path.
You can create a service and configure prometheus to scrape the metrics
endpoint automatically using service annotations.

Metrics configuration values:
| Key                   | Required? | Description                                | Default     |
| --------------------- | --------- | ------------------------------------------ | ----------- |
| `service.create`      | No        | Crete a service for the metrics endpoint.  | `false`     |
| `service.type`        | No        | Type of service metrics enpoint.           | `ClusterIP` |
| `service.annotations` | No        | Annotations to add to the metrics endpont. | Empty       |

You can configure prometheus to scrape the service endpoint automatically by
adding the following annotations to the service (in `my-values.yaml`):
```yaml
service:
  create: true
  annotations:
    prometheus.io/scrape: "true"
    prometheus.io/path: /metrics
    prometheus.io/port: "8080"
```

#### Service Account
If you need to, you can create a kubernetes service account for use with
`update-route53` pods using the following configuration variables:

| Key                     | Required? | Description                                          | Default                                 |
| ----------------------- | --------- | ---------------------------------------------------- | --------------------------------------- |
| `serviceAccount.create` | No        | Create a kubernetes service account for the release. | `false`                                 |
| `serviceAccount.name`   | No        | Name of the service account to use                   | Defaults to the full helm release name. |
