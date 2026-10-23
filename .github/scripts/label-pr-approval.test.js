const assert = require('node:assert/strict');
const { test } = require('node:test');
const labelPRApproval = require('./label-pr-approval.js');

function review(state, login = 'alice') {
  return { state, author: login ? { login } : null };
}

function pullRequest(number, reviews = [], labels = []) {
  return {
    number,
    state: 'open',
    head: { ref: 'feature', repo: { id: 2, owner: { login: 'contributor' } } },
    reviews,
    labels: new Set(labels),
  };
}

function createRepository(prs) {
  const github = {
    rest: {
      pulls: {
        get: async ({ pull_number }) => {
          const pr = prs.find(pr => pr.number === pull_number);
          return { data: { ...pr, labels: [...pr.labels].map(name => ({ name })) } };
        },
        list: async ({ state, head, page, per_page }) => {
          const matching = prs.filter(pr => pr.state === state &&
            (!head || `${pr.head.repo.owner.login}:${pr.head.ref}` === head));
          return { data: matching.slice((page - 1) * per_page, page * per_page) };
        },
      },
      issues: {
        addLabels: async ({ issue_number, labels }) => {
          const pr = prs.find(pr => pr.number === issue_number);
          for (const label of labels) {
            pr.labels.add(label);
          }
        },
        removeLabel: async ({ issue_number, name }) => {
          const pr = prs.find(pr => pr.number === issue_number);
          if (!pr.labels.delete(name)) {
            throw new Error(`Label ${name} is not present`);
          }
        },
      },
    },
    paginate: async (method, parameters) => {
      const results = [];
      let page = 1;
      let data;

      do {
        ({ data } = await method({ ...parameters, page }));
        results.push(...data);
        page++;
      } while (data.length === parameters.per_page);

      return results;
    },
    graphql: async (_query, { number, cursor }) => {
      const pr = prs.find(pr => pr.number === number);
      const start = Number(cursor);
      return {
        repository: {
          pullRequest: {
            latestOpinionatedReviews: {
              nodes: pr.reviews.slice(start, start + 100),
              pageInfo: {
                hasNextPage: start + 100 < pr.reviews.length,
                endCursor: String(start + 100),
              },
            },
          },
        },
      };
    },
  };

  return {
    github,
    label: (eventName = 'pull_request_target', payload = { pull_request: prs[0] }) =>
      labelPRApproval({
        github,
        context: { repo: { owner: 'test', repo: 'test' }, eventName, payload },
        core: { info() {} },
      }),
  };
}

test('labels any current approval except CodeRabbit', async t => {
  const cases = [
    ['no reviews', [], false],
    ['changes requested', [review('CHANGES_REQUESTED')], false],
    ['CodeRabbit only', [review('APPROVED', 'coderabbitai')], false],
    ['approval', [review('APPROVED')], true],
    ['approval despite another objection', [
      review('APPROVED'), review('CHANGES_REQUESTED', 'bob'),
    ], true],
    ['approval alongside CodeRabbit', [
      review('APPROVED', 'coderabbitai'), review('APPROVED'),
    ], true],
    ['another bot approval', [review('APPROVED', 'other-bot')], true],
    ['deleted reviewer', [review('APPROVED', null)], true],
  ];

  for (const [name, reviews, approved] of cases) {
    await t.test(name, async () => {
      const pr = pullRequest(1, reviews, ['size/M', 'kind/bug']);
      const repository = createRepository([pr]);
      await repository.label();
      await repository.label();

      const expected = ['size/M', 'kind/bug'];
      if (approved) {
        expected.push('has-approval');
      }
      assert.deepEqual(pr.labels, new Set(expected));
    });
  }
});

test('removes the label when the last approval is replaced or dismissed', async t => {
  for (const reviews of [[review('CHANGES_REQUESTED')], []]) {
    await t.test(reviews.length ? 'changes requested' : 'dismissed', async () => {
      const pr = pullRequest(1, [review('APPROVED')], ['size/S']);
      const repository = createRepository([pr]);
      await repository.label();
      assert.ok(pr.labels.has('has-approval'));

      pr.reviews = reviews;
      await repository.label();
      await repository.label();
      assert.deepEqual(pr.labels, new Set(['size/S']));
    });
  }
});

test('keeps the label while another reviewer still approves', async () => {
  const pr = pullRequest(1, [review('APPROVED'), review('APPROVED', 'bob')]);
  const repository = createRepository([pr]);
  await repository.label();

  pr.reviews = [review('APPROVED', 'bob')];
  await repository.label();
  assert.ok(pr.labels.has('has-approval'));
});

test('finds approvals beyond the first page and removes the label when they are withdrawn', async () => {
  const reviews = Array.from({ length: 100 }, (_, i) => review('CHANGES_REQUESTED', `user${i}`));
  const pr = pullRequest(1, [...reviews, review('APPROVED')]);
  const repository = createRepository([pr]);
  await repository.label();
  assert.ok(pr.labels.has('has-approval'));

  pr.reviews = [...reviews, review('CHANGES_REQUESTED')];
  await repository.label();
  assert.ok(!pr.labels.has('has-approval'));
});

test('refreshes all open PRs across pages on manual dispatch', async () => {
  const prs = Array.from({ length: 280 }, (_, i) =>
    pullRequest(i + 1, i % 2 ? [] : [review('APPROVED')], ['has-approval', 'size/S']));
  prs[0].state = 'closed';

  await createRepository(prs).label('workflow_dispatch', {});

  for (const pr of prs) {
    assert.equal(pr.labels.has('has-approval'), pr.number % 2 === 1);
    assert.ok(pr.labels.has('size/S'));
  }
});

test('labels all PRs from the triggering source branch and leaves other PRs untouched', async t => {
  for (const eventName of ['pull_request_target', 'workflow_run']) {
    await t.test(eventName, async () => {
      const prs = Array.from({ length: 4 }, (_, i) => pullRequest(i + 1, [review('APPROVED')]));
      prs[1].head = { ...prs[1].head, ref: 'unrelated' };
      prs[2].head = { ...prs[2].head, repo: { ...prs[2].head.repo, id: 3 } };

      const payload = eventName === 'pull_request_target' ? { pull_request: prs[0] } : {
        workflow_run: {
          head_repository: { id: 2, owner: { login: 'contributor' } },
          head_branch: 'feature',
          pull_requests: [],
        },
      };
      await createRepository(prs).label(eventName, payload);

      assert.deepEqual(prs.map(pr => pr.labels.has('has-approval')), [true, false, false, true]);
    });
  }
});

test('leaves closed PRs alone', async () => {
  const pr = pullRequest(1, [], ['has-approval']);
  pr.state = 'closed';
  await createRepository([pr]).label();
  assert.ok(pr.labels.has('has-approval'));
});

test('preserves labels when fetching approval state fails', async () => {
  const pr = pullRequest(1, [], ['has-approval', 'size/M']);
  const repository = createRepository([pr]);
  repository.github.graphql = async () => {
    throw new Error('API unavailable');
  };

  await assert.rejects(repository.label(), /API unavailable/);
  assert.deepEqual(pr.labels, new Set(['has-approval', 'size/M']));
});
