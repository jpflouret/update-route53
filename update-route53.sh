#!/bin/bash
set -e

AWS=/usr/local/bin/aws

if [ ! -x $AWS ];
then
  echo "aws cli not found" >&2
  exit 1
fi

if [[ -z "$HOSTED_ZONE_ID" ]];
then
  echo "Missing environment variable HOSTED_ZONE_ID" >&2
  exit 1
fi

if [[ -z "$DNS_NAME" ]];
then
  echo "Missing environment variable DNS_NAME" >&2
  exit 1
fi

if [[ -z "$DNS_TTL" ]];
then
  DNS_TTL=300
fi

if [[ -z "$AWS_DEFAULT_REGION" ]];
then
  export AWS_DEFAULT_REGION=us-west-2
fi

if [[ -z "$AWS_ACCESS_KEY_ID" ]];
then
  echo "Missing environment variable AWS_ACCESS_KEY_ID" >&2
  exit 1
fi

if [[ -z "$AWS_SECRET_ACCESS_KEY" ]];
then
  echo "Missing environment variable AWS_SECRET_ACCESS_KEY" >&2
  exit 1
fi

if [[ -z "$SLEEP_PERIOD" ]];
then
  SLEEP_PERIOD=5m
fi

if [[ -z "$CHECK_IP" ]];
then
  CHECK_IP=http://checkip.amazonaws.com/
fi

function valid_ip()
{
  local  ip=$1
  local  stat=1

  if [[ $ip =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
    OIFS=$IFS
    IFS='.'
    ip=($ip)
    IFS=$OIFS
    [[ ${ip[0]} -le 255 && ${ip[1]} -le 255 \
      && ${ip[2]} -le 255 && ${ip[3]} -le 255 ]]
    stat=$?
  fi
  return $stat
}

function update_route53()
{
  MY_IP=$(curl -s $CHECK_IP)
  if ! valid_ip $MY_IP;
  then
    echo "Got invalid address '$MY_IP' from $CHECK_IP." >&2
    return
  fi

  CURRENT_IP=$($AWS route53 list-resource-record-sets --hosted-zone-id $HOSTED_ZONE_ID --query "ResourceRecordSets[?Name == '${DNS_NAME}.'].ResourceRecords[0].Value" --output text)
  if ! valid_ip $CURRENT_IP;
  then
    echo "Got invalid address for recordset ${DNS_NAME} for hosted zone ${HOSTED_ZONE_ID}." >&2
    return
  fi

  if [ "$MY_IP" = "$CURRENT_IP" ];
  then
    echo "Current IP address ${MY_IP} matches recordset ${DNS_NAME} for hosted zone ${HOSTED_ZONE_ID}. Done."
    return
  fi

  echo "Updating recordset $DNS_NAME A $MY_IP TTL $DNS_TTL."

  CHANGE_BATCH_FILE=$(mktemp)
  trap "rm -rf $CHANGE_BATCH_FILE" EXIT
  cat << EOF > $CHANGE_BATCH_FILE
  {
    "Changes": [{
      "Action": "UPSERT",
      "ResourceRecordSet": {
        "Name": "$DNS_NAME",
        "Type": "A",
        "TTL": $DNS_TTL,
        "ResourceRecords": [{
          "Value": "$MY_IP"
        }]
      }
    }]
  }
EOF

  CHANGE_ID=$($AWS route53 change-resource-record-sets \
    --hosted-zone-id $HOSTED_ZONE_ID \
    --change-batch file://$CHANGE_BATCH_FILE \
    --output text --query ChangeInfo.Id)

  rm $CHANGE_BATCH_FILE

  echo "Recordset $DNS_NAME updated A $MY_IP with change id $CHANGE_ID"

  echo "Waiting 30 seconds before checking the aws change..."

  sleep 30

  echo "Waiting for change id $CHANGE_ID to complete..."

  $AWS route53 wait resource-record-sets-changed --id "$CHANGE_ID"

  echo "Change $CHANGE_ID complete. Success."
}

while true
do
  update_route53
  sleep $SLEEP_PERIOD
done
