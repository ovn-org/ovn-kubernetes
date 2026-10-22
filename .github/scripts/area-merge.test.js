const assert = require('node:assert/strict');
const fs = require('node:fs');
const { test } = require('node:test');
const areaMerge = require('./area-merge');

/**
 * Build an approved PR fixture with mutable CI state and mocked GitHub APIs.
 * The test context restores the CODEOWNERS read stub after each test.
 */
function fixture(t, eventName = 'workflow_run') {
  t.mock.method(fs, 'readFileSync', () =>
    '# Test area (Area Maintainer: @maintainer)\n/test/e2e/ @maintainer\n');
  const botLogin = process.env.BOT_LOGIN || 'ovn-kubernetes-merge-bot[bot]';
  const headRepository = { id: 1234, owner: { login: 'author' } };
  const pr = {
    number: 6981, state: 'open', user: { login: 'author' },
    head: { sha: 'approved-sha', ref: 'test-branch', repo: headRepository },
  };
  const command = { user: { login: 'maintainer', type: 'User' }, body: '/area-maintainer-approved' };
  const comments = [command, {
    user: { login: botLogin, type: 'Bot' },
    body: '<!-- area-merge-sha:approved-sha -->',
  }];
  const checks = [{ name: 'e2e', status: 'completed', conclusion: 'success' }];
  const status = { state: 'success', statuses: [] };
  const source = {
    id: 123, event: 'pull_request', head_sha: pr.head.sha, pull_requests: [],
    head_repository: headRepository, head_branch: pr.head.ref,
  };
  const associatedPRs = [pr];
  const branchPRs = [pr];
  const context = {
    eventName,
    repo: { owner: 'ovn-kubernetes', repo: 'ovn-kubernetes' },
    sha: 'default-branch-sha',
    payload: { [eventName]: source, comment: command, issue: { number: pr.number } },
  };
  const github = {
    rest: {
      repos: {
        listPullRequestsAssociatedWithCommit: t.mock.fn(),
        getCombinedStatusForRef: async () => ({ data: status }),
      },
      pulls: {
        list: t.mock.fn(),
        get: t.mock.fn(async () => ({ data: pr })),
        listFiles: t.mock.fn(async () => ({ data: [{ filename: 'test/e2e/kubevirt.go' }] })),
        merge: t.mock.fn(async () => ({ data: { merged: true } })),
      },
      issues: {
        listComments: async () => ({ data: comments }),
        createComment: t.mock.fn(async ({ body }) => {
          comments.push({ user: { login: botLogin, type: 'Bot' }, body });
        }),
      },
      checks: {
        listForRef: t.mock.fn(async () => ({ data: { check_runs: checks, total_count: checks.length } })),
      },
    },
    paginate: t.mock.fn(async endpoint => {
      if (endpoint === github.rest.repos.listPullRequestsAssociatedWithCommit) return associatedPRs;
      assert.equal(endpoint, github.rest.pulls.list);
      return branchPRs;
    }),
  };
  const core = { info() {}, setFailed: message => assert.fail(message) };
  return {
    pr, source, command, comments, checks, status, context, github, associatedPRs, branchPRs,
    merge: github.rest.pulls.merge,
    run: () => areaMerge({ github, context, core }),
  };
}

test('fork approval waits for CI, then workflow completion merges the approved SHA', async t => {
  const f = fixture(t, 'issue_comment');
  f.comments.pop();
  f.checks[0].status = 'in_progress';
  await f.run();
  assert.equal(f.merge.mock.callCount(), 0);
  assert.match(f.comments[1].body, /<!-- area-merge-sha:approved-sha -->/);

  f.checks[0].status = 'completed';
  f.context.eventName = 'workflow_run';
  f.context.payload.workflow_run = f.source;
  await f.run();
  assert.equal(f.merge.mock.callCount(), 1);
  assert.equal(f.merge.mock.calls[0].arguments[0].sha, 'approved-sha');
  const [endpoint, params] = f.github.paginate.mock.calls[0].arguments;
  assert.equal(endpoint, f.github.rest.repos.listPullRequestsAssociatedWithCommit);
  assert.equal(params.commit_sha, 'approved-sha');
  assert.equal(params.per_page, 100);
});

for (const eventName of ['workflow_run', 'check_suite']) {
  test(`${eventName} resolves fork PRs by SHA`, async t => {
    const f = fixture(t, eventName);
    await f.run();
    assert.equal(f.github.paginate.mock.callCount(), 1);
    assert.equal(f.merge.mock.callCount(), 1);
  });

  test(`${eventName} uses payload PRs when present`, async t => {
    const f = fixture(t, eventName);
    f.source.pull_requests = [{ number: f.pr.number }];
    await f.run();
    assert.equal(f.github.paginate.mock.callCount(), 0);
    assert.equal(f.merge.mock.callCount(), 1);
  });

  test(`${eventName} requires a waiting marker for the current head`, async t => {
    const f = fixture(t, eventName);
    f.comments[1].body = '<!-- area-merge-sha:old-sha -->';
    await f.run();
    assert.equal(f.merge.mock.callCount(), 0);
    assert.equal(f.github.rest.checks.listForRef.mock.callCount(), 0);
  });
}

test('workflow completion without a waiting marker cannot initiate a merge', async t => {
  const f = fixture(t);
  f.comments.pop();
  await f.run();
  assert.equal(f.merge.mock.callCount(), 0);
});

test('workflow completion resolves a fork PR by branch when commit lookup is empty', async t => {
  const f = fixture(t);
  f.associatedPRs.length = 0;
  await f.run();
  assert.equal(f.github.paginate.mock.callCount(), 2);
  const [endpoint, params] = f.github.paginate.mock.calls[1].arguments;
  assert.equal(endpoint, f.github.rest.pulls.list);
  assert.deepEqual(params, {
    owner: 'ovn-kubernetes', repo: 'ovn-kubernetes',
    state: 'open', head: 'author:test-branch', per_page: 100,
  });
  assert.equal(f.merge.mock.callCount(), 1);
  assert.equal(f.merge.mock.calls[0].arguments[0].sha, 'approved-sha');
});

for (const [field, value] of [['repo', { id: 5678 }], ['ref', 'other-branch'], ['sha', 'old-sha']]) {
  test(`branch discovery rejects a candidate with a different head ${field}`, async t => {
    const f = fixture(t);
    f.associatedPRs.length = 0;
    f.branchPRs[0] = { ...f.pr, head: { ...f.pr.head, [field]: value } };
    await f.run();
    assert.equal(f.github.rest.pulls.get.mock.callCount(), 0);
    assert.equal(f.merge.mock.callCount(), 0);
  });
}

for (const field of ['head_repository', 'head_branch']) {
  test(`branch discovery skips an event without ${field}`, async t => {
    const f = fixture(t);
    f.associatedPRs.length = 0;
    delete f.source[field];
    await f.run();
    assert.equal(f.github.paginate.mock.callCount(), 1);
    assert.equal(f.merge.mock.callCount(), 0);
  });
}

test('branch discovery still requires a bot waiting marker', async t => {
  const f = fixture(t);
  f.associatedPRs.length = 0;
  f.comments.pop();
  await f.run();
  assert.equal(f.github.paginate.mock.callCount(), 2);
  assert.equal(f.merge.mock.callCount(), 0);
});

test('a human cannot forge the bot waiting marker', async t => {
  const f = fixture(t);
  f.comments[1].user = f.command.user;
  await f.run();
  assert.equal(f.merge.mock.callCount(), 0);
});

test('an older workflow completion cannot retry a newer PR head', async t => {
  const f = fixture(t);
  f.source.head_sha = 'old-sha';
  await f.run();
  assert.equal(f.merge.mock.callCount(), 0);
  assert.equal(f.github.rest.checks.listForRef.mock.callCount(), 0);
});

test('SHA discovery ignores closed PRs', async t => {
  const f = fixture(t);
  f.pr.state = 'closed';
  f.branchPRs.length = 0;
  await f.run();
  assert.equal(f.github.rest.pulls.get.mock.callCount(), 0);
  assert.equal(f.merge.mock.callCount(), 0);
});

test('a queued retry skips a PR merged by the preceding run', async t => {
  const f = fixture(t);
  f.source.pull_requests = [{ number: f.pr.number }];
  f.merge.mock.mockImplementation(async () => {
    f.pr.state = 'closed';
    return { data: { merged: true } };
  });
  await f.run();
  await f.run();
  assert.equal(f.github.rest.pulls.get.mock.callCount(), 2);
  assert.equal(f.merge.mock.callCount(), 1);
  assert.equal(f.github.rest.issues.createComment.mock.callCount(), 1);
});

test('workflow completions from pushes are ignored', async t => {
  const f = fixture(t);
  f.source.event = 'push';
  await f.run();
  assert.equal(f.github.paginate.mock.callCount(), 0);
  assert.equal(f.merge.mock.callCount(), 0);
});

test('workflow completion rechecks CI rather than trusting its conclusion', async t => {
  const f = fixture(t);
  f.source.conclusion = 'success';
  f.checks.push({ name: 'performance', status: 'in_progress' });
  await f.run();
  assert.equal(f.merge.mock.callCount(), 0);
  assert.equal(f.github.rest.issues.createComment.mock.callCount(), 0);
});

test('a failing check blocks the retry', async t => {
  const f = fixture(t);
  f.checks[0].conclusion = 'failure';
  await f.run();
  assert.equal(f.merge.mock.callCount(), 0);
  assert.match(f.comments.at(-1).body, /CI checks have failed/);
});

test('a failing commit status blocks the retry', async t => {
  const f = fixture(t);
  f.status.state = 'failure';
  f.status.statuses = [{ context: 'external-ci', state: 'failure' }];
  await f.run();
  assert.equal(f.merge.mock.callCount(), 0);
  assert.match(f.comments.at(-1).body, /external-ci/);
});

for (const eventName of ['workflow_run', 'check_suite']) {
  test(`${eventName} reports the same failures only once regardless of order`, async t => {
    const f = fixture(t, eventName);
    f.checks[0].conclusion = 'failure';
    f.checks.push({ name: 'unit', status: 'completed', conclusion: 'failure' });
    await f.run();
    assert.match(f.comments.at(-1).body, /- `e2e`\n- `unit`/);
    assert.match(f.comments.at(-1).body, /will merge automatically when all checks pass/);
    f.checks.reverse();
    await f.run();
    assert.equal(f.github.rest.issues.createComment.mock.callCount(), 1);
    assert.equal(f.merge.mock.callCount(), 0);

    for (const check of f.checks) check.conclusion = 'success';
    await f.run();
    assert.equal(f.merge.mock.callCount(), 1);
  });
}

test('retries report changed failures and failures on a newly approved SHA', async t => {
  const f = fixture(t);
  f.checks[0].conclusion = 'failure';
  await f.run();
  f.status.state = 'failure';
  f.status.statuses = [{ context: 'external-ci', state: 'failure' }];
  await f.run();
  assert.equal(f.github.rest.issues.createComment.mock.callCount(), 2);
  assert.match(f.comments.at(-1).body, /external-ci/);

  f.pr.head.sha = f.source.head_sha = 'new-approved-sha';
  f.comments[1].body = '<!-- area-merge-sha:new-approved-sha -->';
  await f.run();
  assert.equal(f.github.rest.issues.createComment.mock.callCount(), 3);
  assert.equal(f.merge.mock.callCount(), 0);
});

test('a human failure marker cannot suppress a retry notification', async t => {
  const f = fixture(t);
  f.checks[0].conclusion = 'failure';
  await f.run();
  f.comments.at(-1).user = f.command.user;
  await f.run();
  assert.equal(f.github.rest.issues.createComment.mock.callCount(), 2);
  assert.equal(f.merge.mock.callCount(), 0);
});

for (const waiting of [false, true]) {
  test(`explicit commands always report failures with waiting=${waiting}`, async t => {
    const f = fixture(t, 'issue_comment');
    if (!waiting) f.comments.pop();
    f.checks[0].conclusion = 'failure';
    await f.run();
    await f.run();
    assert.equal(f.github.rest.issues.createComment.mock.callCount(), 2);
    assert.equal(f.merge.mock.callCount(), 0);
    if (waiting) {
      assert.match(f.comments.at(-1).body, /will merge automatically when all checks pass/);
    } else {
      assert.match(f.comments.at(-1).body, /Please fix the failures and run `\/area-maintainer-approved` again/);
      assert.doesNotMatch(f.comments.at(-1).body, /will merge automatically/);
    }
  });
}

test('workflow completion rechecks area authorization', async t => {
  const f = fixture(t);
  f.command.user.login = 'other-reviewer';
  await f.run();
  assert.equal(f.merge.mock.callCount(), 0);
  assert.match(f.comments.at(-1).body, /not listed as an Area Maintainer/);
});

test('workflow completion cannot use the PR author as approver', async t => {
  const f = fixture(t);
  f.pr.user.login = 'maintainer';
  await f.run();
  assert.equal(f.merge.mock.callCount(), 0);
});
