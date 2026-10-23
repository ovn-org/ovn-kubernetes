async function pullRequestsToLabel({ github, context }) {
  if (context.eventName === 'workflow_dispatch') {
    const prs = await github.paginate(github.rest.pulls.list, {
      ...context.repo,
      state: 'open',
      per_page: 100,
    });
    return prs.map(pr => pr.number);
  }

  let source;
  if (context.eventName === 'pull_request_target') {
    source = context.payload.pull_request.head;
  } else {
    const run = context.payload.workflow_run;
    source = { repo: run.head_repository, ref: run.head_branch };
  }

  // Fork workflow runs can have an empty pull_requests array. Resolve the
  // source branch through the API, including PRs targeting different branches.
  // Runs for the same source branch can replace each other's pending runs.
  // Reconcile all matching PRs so none of those updates are lost.
  const prs = await github.paginate(github.rest.pulls.list, {
    ...context.repo,
    state: 'open',
    head: `${source.repo.owner.login}:${source.ref}`,
    per_page: 100,
  });
  return prs
    .filter(pr => pr.head.repo?.id === source.repo.id)
    .map(pr => pr.number);
}

async function hasApproval(github, repo, number) {
  let cursor = null;

  do {
    const { repository } = await github.graphql(`
      query($owner: String!, $repo: String!, $number: Int!, $cursor: String) {
        repository(owner: $owner, name: $repo) {
          pullRequest(number: $number) {
            latestOpinionatedReviews(first: 100, after: $cursor) {
              nodes { state author { login } }
              pageInfo { hasNextPage endCursor }
            }
          }
        }
      }
    `, { ...repo, number, cursor });

    // GitHub supplies the current verdict per reviewer: comments do not undo
    // approvals, and superseded or dismissed approvals do not count.
    const reviews = repository.pullRequest.latestOpinionatedReviews;
    if (reviews.nodes.some(review =>
      review.state === 'APPROVED' && review.author?.login !== 'coderabbitai'
    )) {
      return true;
    }

    cursor = reviews.pageInfo.hasNextPage ? reviews.pageInfo.endCursor : null;
  } while (cursor);

  return false;
}

module.exports = async function labelPRApproval({ github, context, core }) {
  const numbers = await pullRequestsToLabel({ github, context });

  for (const number of numbers) {
    const { data: pr } = await github.rest.pulls.get({
      ...context.repo,
      pull_number: number,
    });
    if (pr.state !== 'open') {
      continue;
    }

    const approved = await hasApproval(github, context.repo, number);
    const label = 'has-approval';
    const labeled = pr.labels.some(existing => existing.name === label);

    // GitHub manages PR labels through the Issues API, using the PR number.
    const issue = { ...context.repo, issue_number: number };

    core.info(`#${number}: ${approved ? 'has' : 'has no'} approval besides CodeRabbit`);
    if (approved && !labeled) {
      await github.rest.issues.addLabels({ ...issue, labels: [label] });
    } else if (!approved && labeled) {
      await github.rest.issues.removeLabel({ ...issue, name: label });
    }
  }
};
