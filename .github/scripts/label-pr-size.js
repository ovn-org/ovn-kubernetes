function isModelgenOutput(filename) {
  // Output directories are defined in go-controller/hack/update-modelgen.sh.
  // Keep schema inputs and generator directives in the size count.
  return /^go-controller\/pkg\/(nbdb|sbdb|vswitchd)\/[^/]+\.go$/.test(filename) &&
    !filename.endsWith('/gen.go');
}

function isIgnoredFile(filename) {
  // CRD output paths are defined in go-controller/hack/update-codegen.sh.
  // Files under apis/ can lack "generated" in their names.
  return /(^|\/)go\.sum$/.test(filename) ||
    filename.split('/').includes('vendor') ||
    isModelgenOutput(filename) ||
    /^helm\/ovn-kubernetes\/crds\/k8s\.ovn\.org_[^/]+\.yaml$/.test(filename) ||
    /(^|\/)[^/]*generated[^/]*\.go$/.test(filename) ||
    /^go-controller\/pkg\/crd\/[^/]+\/[^/]+\/apis\//.test(filename);
}

async function labelPRSize({ github, context, core }, number) {
  const pullRequest = { ...context.repo, pull_number: number };
  const { data: pr } = await github.rest.pulls.get(pullRequest);
  if (pr.state !== 'open') {
    return;
  }

  const files = await github.paginate(github.rest.pulls.listFiles, {
    ...pullRequest,
    per_page: 100,
  });

  const changedLines = files
    .filter(file => !isIgnoredFile(file.filename))
    .reduce((total, file) => total + file.additions + file.deletions, 0);

  // Minimum changed lines for each size, counting additions and deletions.
  const sizes = [
    [1000, 'XXL'],
    [500, 'XL'],
    [100, 'L'],
    [30, 'M'],
    [10, 'S'],
    [0, 'XS'],
  ];
  // The files API caps results at 3,000 files. Use XXL for an incomplete list.
  const complete = files.length === pr.changed_files;
  const size = complete ? sizes.find(([minimum]) => changedLines >= minimum)[1] : 'XXL';
  const label = `size/${size}`;
  const issue = { ...context.repo, issue_number: pr.number };

  core.info(complete
    ? `${changedLines} changed lines: ${label}`
    : `Incomplete file list: ${label}`);

  if (!pr.labels.some(existing => existing.name === label)) {
    await github.rest.issues.addLabels({ ...issue, labels: [label] });
  }

  for (const existing of pr.labels) {
    if (existing.name.startsWith('size/') && existing.name !== label) {
      await github.rest.issues.removeLabel({ ...issue, name: existing.name });
    }
  }
}

module.exports = async function labelPRSizes({ github, context, core }) {
  let numbers;
  if (context.eventName === 'workflow_dispatch') {
    const prs = await github.paginate(github.rest.pulls.list, {
      ...context.repo,
      state: 'open',
      per_page: 100,
    });
    numbers = prs.map(pr => pr.number);
  } else {
    numbers = [context.issue.number];
  }

  for (const number of numbers) {
    await labelPRSize({ github, context, core }, number);
  }
};
