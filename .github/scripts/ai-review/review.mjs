import * as core from '@actions/core';
import * as github from '@actions/github';
import { readFileSync, existsSync } from 'fs';

const GITHUB_TOKEN = process.env.GITHUB_TOKEN;
const LLM_BASE_URL = process.env.LLM_BASE_URL;
const LLM_API_KEY = process.env.LLM_API_KEY;
const LLM_MODEL = process.env.LLM_MODEL;
const PROMPT_FILE = process.env.PROMPT_FILE || 'review-prompt.md';
const CONTEXT_FILES = (process.env.CONTEXT_FILES || '').split(',').map(s => s.trim()).filter(Boolean);
const EXCLUDE_PATTERNS = (process.env.EXCLUDE_PATTERNS || '').split(',').map(s => s.trim()).filter(Boolean);
const MAX_DIFF_CHARS = parseInt(process.env.MAX_DIFF_CHARS || '60000', 10);
const MAX_CONTEXT_FILE_CHARS = parseInt(process.env.MAX_CONTEXT_FILE_CHARS || '20000', 10);
const FAIL_ON_OVERSIZED_DIFF = (process.env.FAIL_ON_OVERSIZED_DIFF || 'true') === 'true';
const TRACKING_MARKER = '<!-- ai-review:tracking -->';

for (const [name, val] of Object.entries({ GITHUB_TOKEN, LLM_BASE_URL, LLM_API_KEY, LLM_MODEL })) {
  if (!val) throw new Error(`Missing required env var: ${name}`);
}

const octokit = github.getOctokit(GITHUB_TOKEN);
const { owner, repo } = github.context.repo;
const pull_number = github.context.payload.pull_request?.number;
if (!pull_number) {
  core.info('Not a pull_request event, skipping.');
  process.exit(0);
}

function globToRegExp(glob) {
  const escaped = glob
    .replace(/[.+^${}()|[\]\\]/g, '\\$&')
    .replace(/\*\*/g, '{{GLOBSTAR}}')
    .replace(/\*/g, '[^/]*')
    .replace(/{{GLOBSTAR}}/g, '.*');
  return new RegExp(`^${escaped}$`);
}
const excludeRegexes = EXCLUDE_PATTERNS.map(globToRegExp);
const isExcluded = (path) => excludeRegexes.some(re => re.test(path));

// Annotate a unified diff patch with new-file line numbers, so both the
// model and GitHub's review-comment API agree on what "line N" means.
function annotatePatch(patch) {
  const lines = patch.split('\n');
  let newLine = 0;
  const out = [];
  for (const line of lines) {
    const hunk = line.match(/^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@/);
    if (hunk) { newLine = parseInt(hunk[1], 10) - 1; continue; }
    if (line.startsWith('+') && !line.startsWith('+++')) {
      newLine += 1;
      out.push(`${newLine}: ${line.slice(1)}`);
    } else if (line.startsWith('-') && !line.startsWith('---')) {
      // removed line — no new-file line number, skip
    } else if (!line.startsWith('\\')) {
      newLine += 1;
      out.push(`${newLine}: ${line.slice(1)}`);
    }
  }
  return out.join('\n');
}

async function getCiStatus(headSha) {
  try {
    const { data } = await octokit.rest.checks.listForRef({ owner, repo, ref: headSha, per_page: 50 });
    if (data.total_count === 0) return 'No checks reported yet.';
    const failing = data.check_runs.filter(c => c.conclusion && !['success', 'skipped', 'neutral'].includes(c.conclusion));
    const pending = data.check_runs.filter(c => !c.conclusion);
    if (failing.length) return `FAILING: ${failing.map(c => c.name).join(', ')}`;
    if (pending.length) return `PENDING: ${pending.map(c => c.name).join(', ')}`;
    return 'All checks green.';
  } catch (e) {
    return `Could not read CI status: ${e.message}`;
  }
}

function readContextFiles() {
  // Context files are repo-root files (AGENTS.md etc.), but the job runs with
  // working-directory set to this script's dir — resolve them against the
  // workspace root, not cwd.
  const root = process.env.GITHUB_WORKSPACE || process.cwd();
  return CONTEXT_FILES
    .filter(f => existsSync(`${root}/${f}`))
    .map(f => {
      let content = readFileSync(`${root}/${f}`, 'utf8');
      if (content.length > MAX_CONTEXT_FILE_CHARS) {
        content = content.slice(0, MAX_CONTEXT_FILE_CHARS) + '\n...[truncated]';
      }
      return `--- ${f} ---\n${content}`;
    })
    .join('\n\n');
}

async function getPreviousRoundNotes() {
  const { data: comments } = await octokit.rest.issues.listComments({ owner, repo, issue_number: pull_number, per_page: 50 });
  const tracking = comments.find(c => c.body?.includes(TRACKING_MARKER));
  return tracking ? tracking.body.replace(TRACKING_MARKER, '').trim() : '(first review round)';
}

// Builds the diff and fails closed (rather than silently truncating) if it's
// too large for a reliable single-pass review. Also flags any single file
// that's oversized on its own, since that's usually an exclude-pattern gap
// (generated code, a big fixture) rather than a genuinely large PR.
async function buildDiff() {
  const files = await octokit.paginate(octokit.rest.pulls.listFiles, { owner, repo, pull_number, per_page: 100 });
  const reviewable = files.filter(f => !isExcluded(f.filename) && f.patch && f.status !== 'removed');

  const annotatedFiles = reviewable.map(file => ({
    filename: file.filename,
    text: `### ${file.filename}\n${annotatePatch(file.patch)}`,
  }));

  const totalChars = annotatedFiles.reduce((sum, f) => sum + f.text.length, 0);
  const oversizedThreshold = Math.floor(MAX_DIFF_CHARS * 0.6);
  const oversizedFiles = annotatedFiles.filter(f => f.text.length > oversizedThreshold);

  if (oversizedFiles.length > 0) {
    return {
      ok: false,
      reason: 'oversized_file',
      totalChars,
      fileCount: annotatedFiles.length,
      offenders: oversizedFiles.map(f => `${f.filename} (~${f.text.length.toLocaleString()} chars)`),
    };
  }

  if (totalChars > MAX_DIFF_CHARS) {
    return {
      ok: false,
      reason: 'oversized_total',
      totalChars,
      fileCount: annotatedFiles.length,
      offenders: annotatedFiles
        .sort((a, b) => b.text.length - a.text.length)
        .slice(0, 5)
        .map(f => `${f.filename} (~${f.text.length.toLocaleString()} chars)`),
    };
  }

  return { ok: true, diff: annotatedFiles.map(f => f.text).join('\n\n') };
}

async function callLlm(prompt) {
  const res = await fetch(`${LLM_BASE_URL.replace(/\/$/, '')}/chat/completions`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${LLM_API_KEY}` },
    body: JSON.stringify({
      model: LLM_MODEL,
      temperature: 0.1,
      max_tokens: 3000,
      messages: [{ role: 'user', content: prompt }],
    }),
  });
  if (!res.ok) throw new Error(`LLM API error ${res.status}: ${await res.text()}`);
  const data = await res.json();
  const raw = data.choices?.[0]?.message?.content?.trim() || '{}';
  const cleaned = raw.replace(/^```json\s*/i, '').replace(/^```\s*/i, '').replace(/```\s*$/i, '');
  try {
    return JSON.parse(cleaned);
  } catch (e) {
    throw new Error(`LLM returned non-JSON: ${e.message} (raw starts: ${raw.slice(0, 60)})`);
  }
}

async function upsertTrackingComment(body) {
  const { data: comments } = await octokit.rest.issues.listComments({ owner, repo, issue_number: pull_number, per_page: 50 });
  const tracking = comments.find(c => c.body?.includes(TRACKING_MARKER));
  if (tracking) {
    await octokit.rest.issues.updateComment({ owner, repo, comment_id: tracking.id, body });
  } else {
    await octokit.rest.issues.createComment({ owner, repo, issue_number: pull_number, body });
  }
}

async function main() {
  const pr = github.context.payload.pull_request;
  const headSha = pr.head.sha;

  const diffResult = await buildDiff();

  if (!diffResult.ok) {
    const isOversizedFile = diffResult.reason === 'oversized_file';
    const guidance = isOversizedFile
      ? `One or more individual files are too large to review reliably on their own. ` +
        `This is usually generated code, a vendored dependency, or a large fixture that ` +
        `should be added to \`EXCLUDE_PATTERNS\` rather than reviewed line-by-line.`
      : `This PR's total reviewable diff is too large for a reliable single-pass review. ` +
        `Please split it into smaller, focused PRs or commits so the review can actually ` +
        `cover the change — a review that silently truncates the diff is worse than no review.`;

    const body = [
      TRACKING_MARKER,
      `### AI Review — skipped`,
      `**Reason:** ${isOversizedFile ? 'oversized file(s)' : 'diff too large'} ` +
        `(~${diffResult.totalChars.toLocaleString()} chars across ${diffResult.fileCount} file(s), limit ${MAX_DIFF_CHARS.toLocaleString()} chars).`,
      `${isOversizedFile ? 'Offending file(s)' : 'Largest files'}:\n${diffResult.offenders.map(f => `- ${f}`).join('\n')}`,
      guidance,
    ].join('\n\n');

    await upsertTrackingComment(body);

    const message = `Diff too large to review reliably (${diffResult.reason}, ~${diffResult.totalChars} chars > ${MAX_DIFF_CHARS} limit).`;
    if (FAIL_ON_OVERSIZED_DIFF) {
      core.setFailed(message);
    } else {
      core.warning(message);
    }
    return;
  }

  const diff = diffResult.diff;

  const [ciStatus, previousRound] = await Promise.all([
    getCiStatus(headSha),
    getPreviousRoundNotes(),
  ]);

  const template = readFileSync(PROMPT_FILE, 'utf8');
  const prompt = template
    .replace('{{REPO}}', `${owner}/${repo}`)
    .replace('{{PR_NUMBER}}', String(pull_number))
    .replace('{{CI_STATUS}}', ciStatus)
    .replace('{{PREVIOUS_ROUND}}', previousRound)
    .replace('{{CONTEXT_FILES}}', readContextFiles())
    .replace('{{DIFF}}', diff);

  const result = await callLlm(prompt);
  const inline = Array.isArray(result.inline) ? result.inline : [];

  // Don't re-flag a path:line already commented on in an earlier round.
  const existingComments = await octokit.paginate(octokit.rest.pulls.listReviewComments, { owner, repo, pull_number, per_page: 100 });
  const seen = new Set(existingComments.map(c => `${c.path}:${c.line}`));
  const newInline = inline.filter(c => !seen.has(`${c.path}:${c.line}`));

  if (newInline.length > 0) {
    await octokit.rest.pulls.createReview({
      owner, repo, pull_number,
      event: 'COMMENT',
      comments: newInline.map(c => ({
        path: c.path,
        line: c.line,
        side: 'RIGHT',
        body: `**[${c.severity}]** ${c.comment}`,
      })),
    });
  }

  const legend = 'Severity: **blocking** = must fix before merge · **important** = should fix in this PR · unlisted items were deferred or omitted as nits.';
  const bodyParts = [
    TRACKING_MARKER,
    `### AI Review — round update`,
    `**CI:** ${ciStatus}`,
    result.summary || '',
    result.readme_note ? `**README:** ${result.readme_note}` : '',
    result.architecture_note ? `**ARCHITECTURE:** ${result.architecture_note}` : '',
    legend,
    `**${result.verdict || 'NEEDS WORK: could not determine verdict'}**`,
  ].filter(Boolean);

  await upsertTrackingComment(bodyParts.join('\n\n'));

  core.info(`Posted ${newInline.length} inline comment(s). Verdict: ${result.verdict}`);
}

main().catch(err => core.setFailed(err.message));
