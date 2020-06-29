const core = require('@actions/core');
const github = require('@actions/github');

async function run() {
  try {  
    const token = core.getInput('token');
    const workflow_id = core.getInput('workflow_id');
    const octokit = github.getOctokit(token);
    const context = github.context;

    console.log(`getting workflow runs for workflow ${workflow_id}`);
    for await (const { data: workflow_runs } of octokit.paginate.iterator(
      octokit.actions.listWorkflowRuns,
      {
        ...context.repo,
        workflow_id: workflow_id,
      })) {
      for (const run of workflow_runs) {
        console.log(`run ${run.id}, event: ${run.event}, status: ${run.status}`)
        if (run.event != "pull_request") {
          console.log(`skipping run ${run.id} as it's not a PR`)
          continue;
        }
        if (run.status == "in_progress" || run.status == "queued") {
          let query = `type:pr+repo:${context.repo.owner}/${context.repo.repo}+author:${run.head_repository.owner.login}+head:${run.head_branch}`;
          console.log(`searching for PRs that match the query: ${query}`)
          const { data: results } = await octokit.search.issuesAndPullRequests({
            q: query,
          });
          if (results.total_count === 0) {
            console.log(`got 0 results for search query`);
            continue;
          }
          let pr_number = results.items[0].number;
          console.log(`getting details of PR #${pr_number}`)
          const { data: pr } = await octokit.pulls.get({
            ...context.repo,
            pull_number: pr_number,
          });
          if (run.head_sha != pr.head.sha) {
            console.log(`cancelling stale run ${run.id} for PR #${pr_number}`);
            await octokit.actions.cancelWorkflowRun({
              ...context.repo,
              run_id: run.id,
            });
          } else {
            console.log(`run ${run.id} is the latest for pr ${pr_number}`)
          }
        } else {
          console.log(`skipping run ${run.id} as it's not in progress/queued`)
        }
      }
    }
  } catch (error) {
    core.setFailed(error.message);
  }
}
run();
