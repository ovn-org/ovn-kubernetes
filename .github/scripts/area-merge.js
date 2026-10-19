// Area Maintainer Merge Bot
//
// Parses CODEOWNERS to determine area maintainer authorization, then verifies
// file scope and CI status before merging PRs via /area-maintainer-approved.
//
// Called from .github/workflows/area-merge.yml via actions/github-script.

const fs = require('fs');

module.exports = async ({ github, context, core }) => {
  // Comments created by this workflow are posted under the GitHub App
  // identity (e.g. "ovn-kubernetes-merge-bot[bot]"), NOT "github-actions[bot]".
  // Unlike the default GITHUB_TOKEN, comments created with an App token DO
  // trigger new issue_comment workflow runs, so the bot must be able to
  // recognize its own comments to avoid reacting to them in a loop.
  const botLogin = process.env.BOT_LOGIN || 'ovn-kubernetes-merge-bot[bot]';

  // --- Determine which PRs to process and who triggered ---
  let pullNumbers = [];
  let commenter = null;
  let retrySha = null;

  if (context.eventName === 'issue_comment') {
    // Defense in depth (the workflow `if` also enforces this): only accept
    // the command at the start of a comment authored by a human. Bot
    // status/error messages merely mentioning the command must never be
    // treated as a merge request.
    const comment = context.payload.comment;
    if (comment.user.type === 'Bot' ||
        !comment.body.trim().startsWith('/area-maintainer-approved')) {
      core.info('Ignoring comment: authored by a bot or not a /area-maintainer-approved command');
      return;
    }
    pullNumbers.push(context.payload.issue.number);
    commenter = comment.user.login.toLowerCase();
  } else if (context.eventName === 'check_suite' || context.eventName === 'workflow_run') {
    const source = context.payload[context.eventName];
    if (context.eventName === 'workflow_run' && source.event !== 'pull_request') {
      core.info('Ignoring workflow run not triggered by a pull request');
      return;
    }
    retrySha = source.head_sha;
    core.info(`Retry from ${context.eventName} ${source.id} for SHA ${retrySha}`);
    for (const pr of (source.pull_requests || [])) {
      pullNumbers.push(pr.number);
    }
    // Fork PRs can have an empty pull_requests array in both event payloads.
    // Use the triggering suite/run SHA, not context.sha (the default branch).
    if (pullNumbers.length === 0 && retrySha) {
      const associatedPRs = await github.paginate(github.rest.repos.listPullRequestsAssociatedWithCommit, {
        owner: context.repo.owner,
        repo: context.repo.repo,
        commit_sha: retrySha,
        per_page: 100,
      });
      pullNumbers = associatedPRs.filter(pr => pr.state === 'open').map(pr => pr.number);
    }
    // The commit association API can also return no results for an open fork
    // PR. Workflow runs retain the source repository and branch in that case.
    if (pullNumbers.length === 0 && context.eventName === 'workflow_run' && retrySha &&
        source.head_repository?.owner?.login && source.head_branch) {
      const candidates = await github.paginate(github.rest.pulls.list, {
        owner: context.repo.owner,
        repo: context.repo.repo,
        state: 'open',
        head: `${source.head_repository.owner.login}:${source.head_branch}`,
        per_page: 100,
      });
      // The head query is owner:branch, so also verify the repository identity.
      pullNumbers = candidates.filter(pr =>
        pr.head.repo?.id === source.head_repository.id &&
        pr.head.ref === source.head_branch && pr.head.sha === retrySha
      ).map(pr => pr.number);
    }
  }

  if (pullNumbers.length === 0) {
    core.info('No pull requests to process');
    return;
  }

  // --- Parse CODEOWNERS ---
  // Extracts two things:
  //   1. Pattern rules: which users are listed on each file pattern line (for review assignment)
  //   2. Area maintainers: parsed from section header comments matching
  //      "Area Maintainer: @user1 @user2" — only these users can trigger merges
  const codeownersContent = fs.readFileSync('CODEOWNERS', 'utf8');
  const rules = [];
  let currentAreaMaintainers = [];

  for (const line of codeownersContent.split('\n')) {
    const trimmed = line.trim();

    // Parse area maintainer(s) from section header comments
    // Format: # ... (Area Maintainer: @user1 @user2)
    const maintainerMatch = trimmed.match(/Area Maintainer[s]?:\s*((?:@[\w-]+[\s,]*)+)/i);
    if (maintainerMatch) {
      currentAreaMaintainers = maintainerMatch[1]
        .match(/@[\w-]+/g)
        .map(u => u.replace('@', '').toLowerCase());
      continue;
    }

    if (!trimmed || trimmed.startsWith('#')) continue;
    const parts = trimmed.split(/\s+/);
    if (parts.length < 2) continue;
    const pattern = parts[0];
    if (pattern === '*') continue;
    const owners = parts.slice(1).map(o => o.replace('@', '').toLowerCase());
    rules.push({ pattern, owners, areaMaintainers: [...currentAreaMaintainers] });
  }

  function matchPattern(pattern, filePath) {
    const normalizedFile = '/' + filePath;
    if (pattern.endsWith('/')) {
      return normalizedFile.startsWith(pattern) ||
             normalizedFile === pattern.slice(0, -1);
    }
    return normalizedFile === pattern;
  }

  // Last matching rule wins, mirroring CODEOWNERS precedence.
  function findAreaMaintainers(filePath) {
    let maintainers = null;
    for (const rule of rules) {
      if (matchPattern(rule.pattern, filePath)) {
        maintainers = rule.areaMaintainers;
      }
    }
    return maintainers;
  }

  async function fetchAllComments(prNumber) {
    const allComments = [];
    let page = 1;
    while (true) {
      const { data: comments } = await github.rest.issues.listComments({
        owner: context.repo.owner,
        repo: context.repo.repo,
        issue_number: prNumber,
        per_page: 100,
        page: page,
      });
      if (comments.length === 0) break;
      allComments.push(...comments);
      if (comments.length < 100) break;
      page++;
    }
    return allComments;
  }

  // --- Process each PR ---
  for (const prNumber of pullNumbers) {
    core.info(`Processing PR #${prNumber}`);

    const { data: pr } = await github.rest.pulls.get({
      owner: context.repo.owner,
      repo: context.repo.repo,
      pull_number: prNumber,
    });

    if (pr.state !== 'open') {
      core.info(`PR #${prNumber} is ${pr.state}, skipping`);
      continue;
    }

    if (context.eventName !== 'issue_comment' && pr.head.sha !== retrySha) {
      core.info(`PR #${prNumber} head ${pr.head.sha} does not match retry SHA ${retrySha}, skipping`);
      continue;
    }

    // Prevent PR authors from merging their own PRs
    const prAuthor = pr.user.login.toLowerCase();
    if (commenter && commenter === prAuthor) {
      core.info(`PR #${prNumber} author ${prAuthor} cannot approve their own PR`);
      await github.rest.issues.createComment({
        owner: context.repo.owner,
        repo: context.repo.repo,
        issue_number: prNumber,
        body: `@${commenter} you cannot use \`/area-maintainer-approved\` on your own PR. ` +
          `Another area maintainer or a repo maintainer must approve and merge it.`,
      });
      continue;
    }

    // For CI completion retries, find who previously ran /area-maintainer-approved
    let requester = commenter;
    let allComments = null;
    if (!requester) {
      allComments = await fetchAllComments(prNumber);
      const mergeComment = [...allComments]
        .reverse()
        .find(c => c.body.trim().startsWith('/area-maintainer-approved') &&
                   c.user.type !== 'Bot' &&
                   c.user.login.toLowerCase() !== prAuthor);
      if (mergeComment) {
        requester = mergeComment.user.login.toLowerCase();
      }
    }

    if (!requester) {
      core.info(`No /area-maintainer-approved request found for PR #${prNumber}, skipping`);
      continue;
    }

    // For CI completion retries, require a bot "waiting" comment for the current head SHA.
    // If new commits were pushed after approval, the SHA won't match and we skip
    // to prevent merging code the area maintainer never reviewed.
    if (context.eventName !== 'issue_comment') {
      if (!allComments) allComments = await fetchAllComments(prNumber);
      const waitingMarker = `<!-- area-merge-sha:${pr.head.sha} -->`;
      const isWaiting = allComments.some(c =>
        c.user.login === botLogin &&
        c.body.includes(waitingMarker)
      );
      if (!isWaiting) {
        core.info(`PR #${prNumber} has no waiting marker for current SHA ${pr.head.sha}, skipping`);
        continue;
      }
    }

    core.info(`/area-maintainer-approved requested by: ${requester}`);

    // Get changed files (paginate for large PRs)
    const changedFiles = [];
    let page = 1;
    while (true) {
      const { data: files } = await github.rest.pulls.listFiles({
        owner: context.repo.owner,
        repo: context.repo.repo,
        pull_number: prNumber,
        per_page: 100,
        page: page,
      });
      if (files.length === 0) break;
      changedFiles.push(...files.map(f => f.filename));
      if (files.length < 100) break;
      page++;
    }

    core.info(`Changed files (${changedFiles.length}): ${changedFiles.join(', ')}`);

    // Check authorization: requester must be an area maintainer for ALL changed files
    const unauthorizedFiles = [];
    for (const file of changedFiles) {
      const maintainers = findAreaMaintainers(file);
      if (!maintainers || !maintainers.includes(requester)) {
        unauthorizedFiles.push(file);
      }
    }

    if (unauthorizedFiles.length > 0) {
      core.info(`Unauthorized files: ${unauthorizedFiles.join(', ')}`);
      await github.rest.issues.createComment({
        owner: context.repo.owner,
        repo: context.repo.repo,
        issue_number: prNumber,
        body: `@${requester} cannot merge this PR via \`/area-maintainer-approved\`.\n\n` +
          `You are not listed as an Area Maintainer for the following files in \`CODEOWNERS\`:\n` +
          unauthorizedFiles.map(f => `- \`${f}\``).join('\n') +
          `\n\nOnly area maintainers (listed in the \`Area Maintainer:\` section header in CODEOWNERS) can trigger merges. ` +
          `A repo maintainer from \`ovn-kubernetes-committers\` is required to merge this PR.`,
      });
      continue;
    }

    // Check CI status
    const ref = pr.head.sha;

    const allCheckRuns = [];
    let checkPage = 1;
    while (true) {
      const { data } = await github.rest.checks.listForRef({
        owner: context.repo.owner,
        repo: context.repo.repo,
        ref: ref,
        per_page: 100,
        page: checkPage,
      });
      allCheckRuns.push(...data.check_runs);
      if (allCheckRuns.length >= data.total_count) break;
      checkPage++;
    }

    // Deduplicate check runs by name, keeping only the most recent per name.
    // GitHub returns all runs for a SHA including stale ones from earlier
    // workflow invocations (e.g. cancelled runs before a close/reopen retry).
    const latestByName = new Map();
    for (const run of allCheckRuns) {
      const existing = latestByName.get(run.name);
      const runTime = new Date(run.started_at || run.created_at || 0).getTime();
      const existingTime = existing ? new Date(existing.started_at || existing.created_at || 0).getTime() : 0;
      if (!existing || runTime > existingTime) {
        latestByName.set(run.name, run);
      }
    }
    const checkRuns = Array.from(latestByName.values());

    const { data: combinedStatus } = await github.rest.repos.getCombinedStatusForRef({
      owner: context.repo.owner,
      repo: context.repo.repo,
      ref: ref,
    });

    const pendingChecks = [];
    const failedChecks = [];

    for (const run of checkRuns) {
      if (run.name === 'area-merge') continue;
      if (run.status !== 'completed') {
        pendingChecks.push(run.name);
      } else if (run.conclusion !== 'success' && run.conclusion !== 'neutral' && run.conclusion !== 'skipped') {
        failedChecks.push(run.name);
      }
    }

    // Gate on the aggregate state first — it covers ALL commit statuses
    // regardless of pagination, so we never miss a failure or pending status.
    // Then iterate the (possibly partial) statuses array for detailed names.
    if (combinedStatus.state === 'failure') {
      for (const status of combinedStatus.statuses) {
        if (status.state === 'failure' || status.state === 'error') {
          failedChecks.push(status.context);
        }
      }
      if (failedChecks.length === 0) {
        failedChecks.push('(commit status failure — details may be on a later page)');
      }
    } else if (combinedStatus.state === 'pending') {
      for (const status of combinedStatus.statuses) {
        if (status.state === 'pending') {
          pendingChecks.push(status.context);
        } else if (status.state === 'failure' || status.state === 'error') {
          failedChecks.push(status.context);
        }
      }
      if (pendingChecks.length === 0 && failedChecks.length === 0) {
        pendingChecks.push('(commit status pending — details may be on a later page)');
      }
    }

    if (failedChecks.length > 0) {
      core.info(`Failed checks: ${failedChecks.join(', ')}`);
      await github.rest.issues.createComment({
        owner: context.repo.owner,
        repo: context.repo.repo,
        issue_number: prNumber,
        body: `Cannot merge via \`/area-maintainer-approved\`: the following CI checks have failed:\n` +
          failedChecks.map(c => `- \`${c}\``).join('\n') +
          `\n\nPlease fix the failures and run \`/area-maintainer-approved\` again.`,
      });
      continue;
    }

    if (pendingChecks.length > 0) {
      core.info(`Pending checks: ${pendingChecks.join(', ')}`);

      const shaMarker = `<!-- area-merge-sha:${ref} -->`;
      if (!allComments) allComments = await fetchAllComments(prNumber);
      const alreadyWaiting = allComments.some(c =>
        c.user.login === botLogin &&
        c.body.includes(shaMarker)
      );
      if (!alreadyWaiting) {
        await github.rest.issues.createComment({
          owner: context.repo.owner,
          repo: context.repo.repo,
          issue_number: prNumber,
          body: `${shaMarker}\nArea maintainer @${requester} has approved this PR via \`/area-maintainer-approved\`. ` +
            `Waiting on CI checks to complete — will merge automatically when all checks pass.\n\n` +
            `Pending checks:\n` +
            pendingChecks.map(c => `- \`${c}\``).join('\n'),
        });
      }
      continue;
    }

    // All checks passed, authorized — merge!
    core.info(`All checks passed. Merging PR #${prNumber}`);
    try {
      await github.rest.pulls.merge({
        owner: context.repo.owner,
        repo: context.repo.repo,
        pull_number: prNumber,
        sha: ref,
        merge_method: 'merge',
        commit_title: `Merge pull request #${prNumber} (/area-maintainer-approved by @${requester})`,
      });

      await github.rest.issues.createComment({
        owner: context.repo.owner,
        repo: context.repo.repo,
        issue_number: prNumber,
        body: `PR merged by area maintainer @${requester} via \`/area-maintainer-approved\`.`,
      });

      core.info(`Successfully merged PR #${prNumber}`);
    } catch (e) {
      core.setFailed(`Failed to merge PR #${prNumber}: ${e.message}`);
      await github.rest.issues.createComment({
        owner: context.repo.owner,
        repo: context.repo.repo,
        issue_number: prNumber,
        body: `Failed to merge this PR: \`${e.message}\`\n\n` +
          `A repo maintainer may need to merge manually.`,
      });
    }
  }
};
