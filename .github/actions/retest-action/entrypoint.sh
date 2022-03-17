#!/bin/sh

set -ex

cat ${GITHUB_EVENT_PATH}

if ! jq -e '.issue.pull_request' ${GITHUB_EVENT_PATH}; then
    echo "Not a PR... Exiting."
    exit 0
fi

COMMENT_BODY=$(jq -r '.comment.body' ${GITHUB_EVENT_PATH})
if [ "${COMMENT_BODY}" != "/retest" ] && [ "${COMMENT_BODY}" != "/retest-failed" ]; then
    echo "Unknown action. Nothing to do... Exiting."
    exit 0
fi
ACTION="${COMMENT_BODY}"

PR_URL=$(jq -r '.issue.pull_request.url' ${GITHUB_EVENT_PATH})

curl --request GET \
    --url "${PR_URL}" \
    --header "authorization: Bearer ${GITHUB_TOKEN}" \
    --header "content-type: application/json" > pr.json

ACTOR=$(jq -r '.user.login' pr.json)
BRANCH=$(jq -r '.head.ref' pr.json)

curl --request GET \
    --url "https://api.github.com/repos/${GITHUB_REPOSITORY}/actions/runs?event=pull_request&actor=${ACTOR}&branch=${BRANCH}" \
    --header "authorization: Bearer ${GITHUB_TOKEN}" \
    --header "content-type: application/json" | jq '.workflow_runs | max_by(.run_number)' > run.json

cat run.json

RERUN_URL=$(jq -r '.rerun_url' run.json)
if [ "${ACTION}" == "/retest-failed" ]; then
  # New feature, rerun failed jobs:
  # https://docs.github.com/en/rest/reference/actions#re-run-failed-jobs-from-a-workflow-run
  RERUN_FAILED_URL=${RERUN_URL}-failed-jobs
fi

# Execute the rerun.
# Store the response code in a variable.
# Store the answer in file .rerun-response.json.
RESPONSE_CODE=$(curl --write-out '%{http_code}' --silent --output .rerun-response.json --request POST \
    --url "${RERUN_URL}" \
    --header "authorization: Bearer ${GITHUB_TOKEN}" \
    --header "content-type: application/json")

REACTION_URL="$(jq -r '.comment.url' ${GITHUB_EVENT_PATH})/reactions"
REACTION_SYMBOL="rocket"
if [[ "${RESPONSE_CODE}" != "2*" ]]; then
  REACTION_SYMBOL="confused"
fi
curl --request POST \
    --url "${REACTION_URL}" \
    --header "authorization: Bearer ${GITHUB_TOKEN}" \
    --header "accept: application/vnd.github.squirrel-girl-preview+json" \
    --header "content-type: application/json" \
    --data '{ "content" : "'${REACTION_SYMBOL}'" }'

PR_COMMENT_URL="${PR_URL}/comments"
if [[ "${RESPONSE_CODE}" != "2*" ]]; then
  RESPONSE_MESSAGE="$(jq -r '.message' .rerun-response.json"
  curl --request POST \
      --url "${PR_COMMENT_URL}" \
      --header "authorization: Bearer ${GITHUB_TOKEN}" \
      --header "accept: application/vnd.github.squirrel-girl-preview+json" \
      --header "content-type: application/json" \
      --data '{ "body" : "Oops, somethind went wrong...: '${RESPONSE_MESSAGE}'" }'
fi
