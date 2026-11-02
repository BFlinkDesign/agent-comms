'use strict';

module.exports = async function ({ github, context, core, admissionOnly = false, expectedHead, setTimeout = globalThis.setTimeout }) {
const HOLD_LABELS = ['hold', 'do-not-merge'];
// Files that decide which checks run and how. Compared in lower case.
const GUARDED = ['.github/workflows/', '.github/actions/', '.pre-commit-config.yaml', '.semgrepignore', '.semgrep.yml', '.semgrep.yaml', '.semgrep/', 'scripts/auto-merge/', '.github/scripts/auto-merge.cjs'];
const CHECK_CONFIG = /(^|\/)(package\.json|pyproject\.toml|pytest\.ini|tox\.ini|setup\.cfg|\.flake8|\.ruff\.toml|ruff\.toml|(?:eslint|vitest|vite|jest|playwright|next|webpack|rollup)\.config\.[cm]?[jt]s|\.eslintrc(?:\.[^/]+)?|tsconfig(?:\.[^/]+)?\.json|biome\.jsonc?)$/;
const GUARD_MARKER = '<!-- auto-merge:guarded -->';
const GREEN = new Set(['success', 'neutral', 'skipped']);
const POLL_MS = 20000;
const MAX_POLLS = 30;
const DISABLE = `mutation($id: ID!) {
  disablePullRequestAutoMerge(input: { pullRequestId: $id }) { clientMutationId }
}`;

const { owner, repo } = context.repo;
const number = context.payload.pull_request.number;
core.setOutput('ready', 'false');
const defaultBranch = context.payload.repository.default_branch;
const held = (labels) => labels.some((label) => HOLD_LABELS.includes(label.name.toLowerCase()));
// Runs can start in a different order than their events happened, so
// decide from the pull request as it is now, not from the event payload.
const { data: pr } = await github.request('GET /repos/{owner}/{repo}/pulls/{pull_number}', {
  owner, repo, pull_number: number,
});
if (expectedHead && pr.head.sha !== expectedHead) return core.info('The admitted head changed; this run cannot merge it.');
if (pr.state !== 'open') return core.info(`#${number} is ${pr.state}.`);

async function switchOff(reason) {
  try {
    await github.graphql(DISABLE, { id: pr.node_id });
    core.notice(`Auto-merge is off for #${number}: ${reason}.`);
  } catch (error) {
    if (!/not in auto.?merge|auto.?merge is not enabled/i.test(error.message)) throw error;
    core.info(`Auto-merge was not on for #${number} (${reason}).`);
  }
}

// Retire requests created by older workflow versions. Native auto-merge
// survives some writer pushes and cannot lease a file-inspected head.
if (pr.auto_merge) await switchOff('using only SHA-pinned immediate merges');

if (pr.draft) return switchOff('it is a draft');
if (pr.base.ref !== defaultBranch) return switchOff(`it targets ${pr.base.ref}, not ${defaultBranch}`);
if (held(pr.labels)) return switchOff('it carries a hold label');

// A person merges a change to the checks themselves: it changes what
// "green" means. So does a PR too large for its file list to be read whole.
async function needsPerson(reason, detail) {
  await switchOff(reason);
  const comments = await github.paginate('GET /repos/{owner}/{repo}/issues/{issue_number}/comments', {
    owner, repo, issue_number: number, per_page: 100,
  });
  if (!comments.some((comment) => (comment.body ?? '').includes(GUARD_MARKER))) {
    await github.request('POST /repos/{owner}/{repo}/issues/{issue_number}/comments', {
      owner, repo, issue_number: number,
      body: `${GUARD_MARKER}\n${detail} Review it and merge it by hand.`,
    });
  }
}
const files = await github.paginate('GET /repos/{owner}/{repo}/pulls/{pull_number}/files', {
  owner, repo, pull_number: number, per_page: 100,
});
if (files.length < pr.changed_files) {
  return needsPerson('its file list is too long to check',
    `This pull request changes ${pr.changed_files} files and GitHub lists only ${files.length}, ` +
    'so it cannot be checked for changes to CI and is not merged automatically.');
}
const guarded = [...new Set(files
  .flatMap((file) => [file.filename, file.previous_filename])
  .filter((path) => typeof path === 'string' && (GUARDED.some((prefix) => path.toLowerCase().startsWith(prefix)) || CHECK_CONFIG.test(path.toLowerCase()))))];
if (guarded.length > 0) {
  return needsPerson('it changes CI',
    `This pull request changes CI (${guarded.join(', ')}), so it is not merged automatically: ` +
    'a change to the checks changes what "green" means.');
}

// Never ask GitHub to merge into a branch that requires no checks:
// with nothing to wait for, auto-merge would merge at once.
const required = new Set();
const rules = await github.paginate('GET /repos/{owner}/{repo}/rules/branches/{branch}', {
  owner, repo, branch: defaultBranch, per_page: 100,
});
for (const rule of rules) {
  if (rule.type !== 'required_status_checks') continue;
  for (const check of rule.parameters?.required_status_checks ?? []) required.add(check.context);
}
try {
  const { data: branch } = await github.request('GET /repos/{owner}/{repo}/branches/{branch}', {
    owner, repo, branch: defaultBranch,
  });
  const classic = branch.protection?.required_status_checks;
  if (classic && classic.enforcement_level !== 'off') {
    for (const name of classic.contexts ?? []) required.add(name);
  }
} catch (error) {
  core.info(`Classic branch protection is not readable here: ${error.message}`);
}
if (required.size === 0) {
  core.warning(
    `${defaultBranch} requires no status checks, so GitHub would merge #${number} without waiting for CI. ` +
    'Auto-merge stays off. Configure the gate with dev-setup scripts/auto-merge/apply.ts.',
  );
  return;
}
const requiredList = [...required].sort().join(', ');

if (admissionOnly) {
  core.setOutput('ready', 'true');
  core.setOutput('head', pr.head.sha);
  return;
}
core.info(`Waiting for all checks on the inspected head; required: ${requiredList}.`);

// Merge only this inspected head, after every
// check and status on the head commit is green — not merely the required
// ones — and has stayed the same across two reads, so a check that starts
// late is seen. Every read also re-checks draft, base and hold labels.
const { data: self } = await github.request('GET /repos/{owner}/{repo}/actions/runs/{run_id}', {
  owner, repo, run_id: context.runId,
});
let previous = '';
for (let poll = 1; ; poll++) {
  const { data: fresh } = await github.request('GET /repos/{owner}/{repo}/pulls/{pull_number}', {
    owner, repo, pull_number: number,
  });
  if (fresh.state !== 'open' || fresh.draft || fresh.base.ref !== defaultBranch || held(fresh.labels)) {
    core.info(`#${number} is no longer an open, ready, unheld pull request into ${defaultBranch}.`);
    return;
  }
  if (fresh.head.sha !== pr.head.sha) {
    return switchOff('its head changed after its files were inspected');
  }
  const sha = fresh.head.sha;
  const listed = await github.paginate('GET /repos/{owner}/{repo}/actions/runs', {
    owner, repo, head_sha: sha, per_page: 100,
  });
  const suites = new Set([self, ...listed.filter((run) => run.workflow_id === self.workflow_id)]
    .map((run) => run.check_suite_id).filter(Number.isInteger));
  const owners = new Map(listed.filter((run) => Number.isInteger(run.check_suite_id))
    .map((run) => [run.check_suite_id, run]));
  const same = (run) => {
    const owner = owners.get(run.check_suite?.id);
    return JSON.stringify(owner ? [owner.workflow_id, owner.event, run.name] : [run.check_suite?.id ?? null, run.name]);
  };
  const checks = (await github.paginate('GET /repos/{owner}/{repo}/commits/{ref}/check-runs', {
    owner, repo, ref: sha, per_page: 100,
  })).filter((run) => !(run.name === 'merge-inspected-head' && suites.has(run.check_suite?.id)));
  // When the run that owns a check last started; NaN when no listed run owns it.
  const started = (run) => Date.parse(owners.get(run.check_suite?.id)?.run_started_at ?? '');
  const ranLater = (cancelled) => checks.some((other) => other.conclusion !== 'cancelled' && other.conclusion !== 'skipped' &&
    same(other) === same(cancelled) && started(other) > started(cancelled));
  const runs = checks.filter((run) => !(run.conclusion === 'cancelled' && ranLater(run)));
  const statuses = await github.paginate('GET /repos/{owner}/{repo}/commits/{ref}/statuses', {
    owner, repo, ref: sha, per_page: 100,
  });
  // The statuses list is newest first and keeps every update; the first
  // entry for each context is its current state.
  const current = new Map();
  for (const status of statuses) if (!current.has(status.context)) current.set(status.context, status.state);
  const red = [
    ...runs.filter((run) => run.status === 'completed' && !GREEN.has(run.conclusion)).map((run) => run.name),
    ...[...current].filter(([, state]) => state === 'failure' || state === 'error').map(([context]) => context),
  ];
  const running = [
    ...runs.filter((run) => run.status !== 'completed').map((run) => run.name),
    ...[...current].filter(([, state]) => state === 'pending').map(([context]) => context),
  ];
  if (red.length > 0) {
    core.warning(`Not merging #${number}: ${red.join(', ')} did not pass on ${sha.slice(0, 7)}.`);
    return;
  }
  const seen = JSON.stringify([sha, runs.map((run) => run.name).sort(), [...current.keys()].sort()]);
  if (running.length > 0 || seen !== previous) {
    if (poll >= MAX_POLLS) {
      core.warning(`Not merging #${number}: ${running.join(', ') || 'its checks'} still changing on ${sha.slice(0, 7)}.`);
      return;
    }
    if (running.length > 0) core.info(`Waiting for ${running.join(', ')} (${poll}/${MAX_POLLS}).`);
    previous = running.length > 0 ? '' : seen;
    await new Promise((resolve) => setTimeout(resolve, POLL_MS));
    continue;
  }
  // Re-check stop conditions at the final boundary, after check reads.
  const { data: finalPr } = await github.rest.pulls.get({
    owner, repo, pull_number: number,
  });
  if (finalPr.state !== 'open' || finalPr.draft ||
      finalPr.base.ref !== defaultBranch || held(finalPr.labels) ||
      finalPr.head.sha !== pr.head.sha) {
    return switchOff('it changed before the merge');
  }
  try {
    await github.request('PUT /repos/{owner}/{repo}/pulls/{pull_number}/merge', {
      owner, repo, pull_number: number, sha, merge_method: 'squash',
    });
    core.notice(`Squash-merged #${number} at ${sha.slice(0, 7)}: every check passed.`);
    return;
  } catch (error) {
    // 409: the head moved after it was read. 405: GitHub's rules
    // refused the merge. Either way the PR now has checks to wait
    // for, so stop: a fresh event must re-inspect its files and requirements.
    if (error.status !== 409 && error.status !== 405) throw error;
    await switchOff('the merge was refused or its head changed');
    core.warning(`Not merging #${number}: re-inspect its current head and requirements.`);
    return;
  }
}
};
