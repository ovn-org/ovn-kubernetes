const assert = require('node:assert/strict');
const { test } = require('node:test');
const labelPRSize = require('./label-pr-size.js');

function changedFile(filename, additions, deletions = 0) {
  return { filename, additions, deletions };
}

function createPullRequest(files, labels = [], changedFiles = files.length) {
  const repositoryLabels = new Set([
    'size/XS', 'size/S', 'size/M', 'size/L', 'size/XL', 'size/XXL', 'kind/bug',
  ]);
  const currentLabels = new Set(labels);
  const github = {
    rest: {
      pulls: {
        get: async () => ({
          data: {
            number: 1,
            changed_files: changedFiles,
            labels: [...currentLabels].map(name => ({ name })),
          },
        }),
        listFiles: async () => ({ data: files }),
      },
      issues: {
        addLabels: async ({ labels }) => {
          for (const label of labels) {
            if (!repositoryLabels.has(label)) {
              throw new Error(`Unknown repository label: ${label}`);
            }

            currentLabels.add(label);
          }
        },
        removeLabel: async ({ name }) => {
          currentLabels.delete(name);
        },
      },
    },
    paginate: async (method, parameters) => (await method(parameters)).data,
  };

  return {
    repositoryLabels,
    labels: currentLabels,
    label: () => labelPRSize({
      github,
      context: { repo: { owner: 'test', repo: 'test' }, issue: { number: 1 } },
      core: { info() {} },
    }),
  };
}

test('assigns sizes at each threshold', async t => {
  const cases = [
    [0, 'XS'], [9, 'XS'],
    [10, 'S'], [29, 'S'],
    [30, 'M'], [99, 'M'],
    [100, 'L'], [499, 'L'],
    [500, 'XL'], [999, 'XL'],
    [1000, 'XXL'], [2000, 'XXL'],
  ];

  for (const [lines, size] of cases) {
    await t.test(`${lines} changed lines`, async () => {
      const pr = createPullRequest([changedFile('source.go', lines)]);
      await pr.label();
      assert.deepEqual([...pr.labels], [`size/${size}`]);
    });
  }
});

test('counts additions and deletions across files', async () => {
  const pr = createPullRequest([
    changedFile('source.go', 10, 10),
    changedFile('deleted.go', 0, 10),
  ]);
  await pr.label();
  assert.deepEqual([...pr.labels], ['size/M']);
});

test('ignores dependency checksums, licenses, vendor code, and generated Go files', async t => {
  const ignoredFiles = [
    'go.sum',
    'go-controller/go.sum',
    'test/e2e/go.sum',
    'test/conformance/go.sum',
    'test/crd-integration/go.sum',
    'LICENSES/packages/dependency/v1.0.0/LICENSE',
    'LICENSES/packages/dependency/v1.0.0/NOTICE',
    'go-controller/vendor/dependency/source.go',
    'go-controller/pkg/crd/egressip/v1/zz_generated.deepcopy.go',
    'go-controller/pkg/crd/egressip/v1/apis/clientset/versioned/clientset.go',
    'go-controller/pkg/nbdb/logical_switch.go',
    'go-controller/pkg/sbdb/chassis.go',
    'go-controller/pkg/vswitchd/model.go',
  ];

  for (const filename of ignoredFiles) {
    await t.test(filename, async () => {
      const pr = createPullRequest([
        changedFile(filename, 1000),
        changedFile('source.go', 10),
      ]);
      await pr.label();
      assert.deepEqual([...pr.labels], ['size/S']);
    });
  }
});

test('counts CRD source changes without generated Helm CRD YAML', async () => {
  const pr = createPullRequest([
    changedFile('go-controller/pkg/crd/egressip/v1/types.go', 5, 5),
    changedFile('helm/ovn-kubernetes/crds/k8s.ovn.org_egressips.yaml', 1000, 1000),
  ]);
  await pr.label();
  assert.deepEqual([...pr.labels], ['size/S']);
});

test('counts module definitions and similarly named documentation', async () => {
  const pr = createPullRequest([
    changedFile('go-controller/go.mod', 10),
    changedFile('docs/go.sum.md', 10),
    changedFile('docs/generated.md', 10),
  ]);
  await pr.label();
  assert.deepEqual([...pr.labels], ['size/M']);
});

test('counts modelgen schemas and generator directives', async t => {
  const countedFiles = [
    'go-controller/pkg/nbdb/ovn-nb.ovsschema',
    'go-controller/pkg/sbdb/ovn-sb.ovsschema',
    'go-controller/pkg/vswitchd/vswitch.ovsschema',
    'go-controller/pkg/nbdb/gen.go',
    'go-controller/pkg/sbdb/gen.go',
    'go-controller/pkg/vswitchd/gen.go',
  ];

  for (const filename of countedFiles) {
    await t.test(filename, async () => {
      const pr = createPullRequest([changedFile(filename, 1000)]);
      await pr.label();
      assert.deepEqual([...pr.labels], ['size/XXL']);
    });
  }
});

test('replaces previous size labels and preserves unrelated labels', async () => {
  const pr = createPullRequest([changedFile('source.go', 30)], [
    'kind/bug', 'size/S', 'size/XL',
  ]);
  await pr.label();
  await pr.label();
  assert.deepEqual([...pr.labels].sort(), ['kind/bug', 'size/M']);
});

test('keeps labels when the correct size is already present', async () => {
  const pr = createPullRequest([changedFile('source.go', 30)], ['size/M', 'kind/bug']);
  await pr.label();
  assert.deepEqual([...pr.labels].sort(), ['kind/bug', 'size/M']);
});

test('preserves PR labels when the new size label is missing from the repository', async () => {
  const pr = createPullRequest([changedFile('source.go', 30)], ['size/S', 'kind/bug']);
  pr.repositoryLabels.delete('size/M');

  await assert.rejects(pr.label(), /Unknown repository label: size\/M/);
  assert.deepEqual([...pr.labels].sort(), ['kind/bug', 'size/S']);
});

test('labels a PR containing only excluded files as XS', async () => {
  const pr = createPullRequest([
    changedFile('go-controller/go.sum', 1000),
    changedFile('LICENSES/packages/dependency/v1.0.0/LICENSE', 0, 1000),
  ]);
  await pr.label();
  assert.deepEqual([...pr.labels], ['size/XS']);
});

test('uses XXL when the file list is incomplete', async () => {
  const pr = createPullRequest([changedFile('source.go', 10)], ['size/XL', 'kind/bug'], 3001);
  await pr.label();
  assert.deepEqual([...pr.labels].sort(), ['kind/bug', 'size/XXL']);
});
