# Update AWS Route 53 Docker Container

This docker container periodically updates a route53 A record with an external
IP address. This is usefull if you have a dynamic IP address and want to update
the current IP in an AWS route 53 record.

## Usage

```
docker run -d \
    --name update-route53 \
    -e AWS_ACCESS_KEY_ID=<your access key id> \
    -e AWS_SECRET_ACCESS_KEY=<your secret access key> \
    -e AWS_DEFAULT_REGION=<your aws region> \
    -e HOSTED_ZONE_ID=<your route53 hoseted zone id> \
    -e DNS_NAME=myhost.domain.com \
    jpflouret/update-route53
```
