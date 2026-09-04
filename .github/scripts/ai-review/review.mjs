import * as core from '@actions/core';
import * as github from '@actions/github';
import { graphql } from '@octokit/graphql';
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
  // Build a regex from a glob pattern; `**` becomes `.*`, `*` becomes `[^/]*`.
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
    // Exclude this job's own check run — it's in_progress (no conclusion) while
    // this code runs, so it always lands in the `pending` bucket and would make
    // every round report "PENDING: AI PR Review" even when real CI is green.
    // ponytail: filters by job name, which also excludes any same-named check
    // run (review bots aren't CI gates anyway); refine by workflow name if a
    // future real check collides on the job name.
    const ownJob = github.context.job;
    const runs = data.check_runs.filter(c => c.name !== ownJob);
    if (runs.length === 0) return 'No checks reported yet.';
    const failing = runs.filter(c => c.conclusion && !['success', 'skipped', 'neutral'].includes(c.conclusion));
    const pending = runs.filter(c => !c.conclusion);
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
  // Each round creates a NEW parent comment, so take the most recent one.
  const tracking = comments
    .filter(c => c.body?.includes(TRACKING_MARKER))
    .sort((a, b) => new Date(b.created_at) - new Date(a.created_at))[0];
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

// flash-class models ignore response_format and answer in prose, but they
// still honor function calling. Force the model into a `review` tool so it
// emits the review as JSON tool-call arguments.
const REVIEW_TOOL = {
  type: 'function',
  function: {
    name: 'review',
    description: 'Report the PR review findings as a single JSON object.',
    parameters: {
      type: 'object',
      properties: {
        verdict: { type: 'string', description: '"MERGE-READY: <reason>" or "NEEDS WORK: <n> blocking, <m> important"' },
        summary: { type: 'string', description: '2-6 sentence prose summary of the review' },
        readme_note: { type: 'string', description: 'one line, or empty string if no README drift' },
        architecture_note: { type: 'string', description: 'one line, or empty string if no ARCHITECTURE drift' },
        inline: {
          type: 'array',
          description: 'blocking/important findings only, each pinned to an exact line in the diff',
          items: {
            type: 'object',
            properties: {
              path: { type: 'string' },
              line: { type: 'integer' },
              severity: { type: 'string', enum: ['blocking', 'important'] },
              comment: { type: 'string', description: 'Specific and actionable. Format for readability with line breaks (short lines or bullet points), not one long paragraph.' },
            },
            required: ['path', 'line', 'severity', 'comment'],
          },
        },
      },
      required: ['verdict', 'summary', 'inline'],
    },
  },
};

async function callLlmOnce(prompt) {
  const res = await fetch(`${LLM_BASE_URL.replace(/\/$/, '')}/chat/completions`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${LLM_API_KEY}` },
    body: JSON.stringify({
      model: LLM_MODEL,
      temperature: 0.1,
      max_tokens: 64000,
      messages: [{ role: 'user', content: prompt }],
      tools: [REVIEW_TOOL],
      tool_choice: { type: 'function', function: { name: 'review' } },
    }),
  });
  if (!res.ok) throw new Error(`LLM API error ${res.status}: ${await res.text()}`);
  const data = await res.json();
  const msg = data.choices?.[0]?.message;
  if (!msg) throw new Error(`LLM returned no message: ${JSON.stringify(data).slice(0, 200)}`);

  // Function calling puts the JSON in tool_calls[].function.arguments; fall
  // back to content (string / array) and then reasoning_content for providers
  // that answer another way. Log the raw output for diagnosis.
  let content = msg.tool_calls?.[0]?.function?.arguments?.trim() || '';
  if (!content.trim()) {
    if (typeof msg.content === 'string') content = msg.content;
    else if (Array.isArray(msg.content)) content = msg.content.map(p => p?.text || '').join('');
  }
  if (!content.trim() && typeof msg.reasoning_content === 'string') content = msg.reasoning_content;
  core.info(`LLM raw (${content.length} chars): ${content.trim().slice(0, 240)}`);

  const raw = content.trim() || '{}';
  const cleaned = raw.replace(/^```json\s*/i, '').replace(/^```\s*/i, '').replace(/```\s*$/i, '');
  let parsed;
  try {
    parsed = JSON.parse(cleaned);
  } catch (e) {
    throw new Error(`LLM returned non-JSON: ${e.message} (raw starts: ${raw.slice(0, 60)})`);
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed) || !parsed.verdict) {
    throw new Error(`LLM response missing verdict (raw: ${raw.slice(0, 120)}). Check the model/prompt.`);
  }
  return parsed;
}

// The flash model intermittently drifts into prose instead of the JSON tool
// call, so retry a few times before failing the run.
async function callLlm(prompt) {
  let lastErr = null;
  for (let attempt = 1; attempt <= 3; attempt++) {
    try {
      return await callLlmOnce(prompt);
    } catch (e) {
      lastErr = e;
      core.warning(`LLM attempt ${attempt}/3 failed: ${e.message}`);
    }
  }
  throw lastErr;
}

// Each commit gets a NEW parent comment. Create it with all checkboxes
// unchecked, then update it (by id) as steps complete.
let parentCommentId = null;
async function createParentComment(body) {
  const { data } = await octokit.rest.issues.createComment({ owner, repo, issue_number: pull_number, body });
  parentCommentId = data.id;
}
async function updateParentComment(body) {
  await octokit.rest.issues.updateComment({ owner, repo, comment_id: parentCommentId, body });
}

// Resolve prior review-comment threads whose finding is no longer flagged in
// this round (the finding was fixed). Thread resolution is GraphQL-only; the
// comment id maps to its thread via a reviewThreads query.
async function resolveFixedThreads(fixedComments) {
  if (!fixedComments.length) return 0;
  // @octokit/graphql authenticates via headers.authorization, not `auth:`.
  const gql = graphql.defaults({ headers: { authorization: `token ${GITHUB_TOKEN}` } });
  let threadQuery;
  try {
    threadQuery = await gql(`query($owner:String!, $repo:String!, $pr:Int!) {
      repository(owner: $owner, name: $repo) {
        pullRequest(number: $pr) {
          reviewThreads(first: 50) {
            nodes {
              id
              isResolved
              comments(first: 20) { nodes { id databaseId } }
            }
          }
        }
      }
    }`, { owner, repo, pr: pull_number });
  } catch (e) {
    core.warning(`Could not read review threads: ${e.message}`);
    return 0;
  }
  const commentToThread = new Map();
  for (const t of threadQuery.repository.pullRequest.reviewThreads.nodes) {
    if (t.isResolved) continue;
    // `id` is the GraphQL node id; `databaseId` is the REST numeric id we match on.
    for (const c of t.comments.nodes) commentToThread.set(c.databaseId, t.id);
  }
  let resolved = 0;
  for (const c of fixedComments) {
    const threadId = commentToThread.get(c.id);
    if (!threadId) continue;
    try {
      await gql(`mutation($threadId: ID!) { resolveReviewThread(input: { threadId: $threadId }) { thread { id } } }`, { threadId });
      resolved += 1;
    } catch (e) {
      core.warning(`Could not resolve comment ${c.id}: ${e.message}`);
    }
  }
  return resolved;
}

const PROGRESS_ITEMS = [
  'Gather context (AGENTS.md, README.md, ARCHITECTURE.md, CI status)',
  'Read changed files',
  'Check CI status',
  'Review for BLOCKING/IMPORTANT issues',
  'Post inline comments',
  'Post final summary with verdict',
];

// The parent comment is a fresh comment per commit; checkboxes start unchecked
// and get checked as the review progresses (like claude's parent).
function progressBody(doneCount, extra) {
  const checklist = PROGRESS_ITEMS.map((p, i) => `${i < doneCount ? '- [x]' : '- [ ]'} ${p}`);
  const parts = [TRACKING_MARKER, '### DeepSeek AI Review', ...checklist];
  if (extra) parts.push(extra);
  return parts.join('\n');
}

async function main() {
  const pr = github.context.payload.pull_request;
  const headSha = pr.head.sha;
  const startedAt = Date.now();

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
      `### DeepSeek AI Review — skipped`,
      `**Reason:** ${isOversizedFile ? 'oversized file(s)' : 'diff too large'} ` +
        `(~${diffResult.totalChars.toLocaleString()} chars across ${diffResult.fileCount} file(s), limit ${MAX_DIFF_CHARS.toLocaleString()} chars).`,
      `${isOversizedFile ? 'Offending file(s)' : 'Largest files'}:\n${diffResult.offenders.map(f => `- ${f}`).join('\n')}`,
      guidance,
    ].join('\n\n');

    await createParentComment(body);

    const message = `Diff too large to review reliably (${diffResult.reason}, ~${diffResult.totalChars} chars > ${MAX_DIFF_CHARS} limit).`;
    if (FAIL_ON_OVERSIZED_DIFF) {
      core.setFailed(message);
    } else {
      core.warning(message);
    }
    return;
  }

  const diff = diffResult.diff;

  // Read the previous round's notes BEFORE creating a NEW parent comment for
  // this commit, then create it with all checkboxes unchecked (the review shows
  // progress as boxes get checked on that comment while the check is running).
  const [ciStatus, previousRound] = await Promise.all([
    getCiStatus(headSha),
    getPreviousRoundNotes(),
  ]);
  await createParentComment(progressBody(0, `**CI:** ${ciStatus}`));

  const template = readFileSync(PROMPT_FILE, 'utf8');
  // Use function replacements: String.replace interprets $&, $', $$ etc. in the
  // replacement string, which corrupts the diff (the code contains '$&'), turning
  // it into '{{DIFF}}'. Functions avoid that substitution.
  const prompt = template
    .replace('{{REPO}}', () => `${owner}/${repo}`)
    .replace('{{PR_NUMBER}}', () => String(pull_number))
    .replace('{{CI_STATUS}}', () => ciStatus)
    .replace('{{PREVIOUS_ROUND}}', () => previousRound)
    .replace('{{CONTEXT_FILES}}', () => readContextFiles())
    .replace('{{DIFF}}', () => diff);

  const result = await callLlm(prompt);
  const inline = Array.isArray(result.inline) ? result.inline : [];

  // Distinguish NEW findings (post as child inline comments) from RE-FLAGGED
  // findings (already commented in a prior round — don't re-post inline, just
  // re-capture the high-level in the parent). GitHub rewrites a prior comment's
  // `commit_id` to the current head when its line persists, so matching on
  // `commit_id === headSha` reliably detects prior-round re-flags.
  const existingComments = await octokit.paginate(octokit.rest.pulls.listReviewComments, { owner, repo, pull_number, per_page: 100 });
  const seen = new Set(existingComments.filter(c => c.commit_id === headSha).map(c => `${c.path}:${c.line}`));
  const priorSeverity = new Map();
  for (const c of existingComments) {
    const m = c.body?.match(/\[(blocking|important)\]/) || [];
    if (m[1]) priorSeverity.set(`${c.path}:${c.line}`, m[1]);
  }
  const newInline = [];
  const reflagged = [];
  for (const c of inline) {
    if (seen.has(`${c.path}:${c.line}`)) reflagged.push(c);
    else newInline.push(c);
  }

  // Prior blocking/important comments whose finding is no longer flagged this
  // round are treated as fixed — resolve their threads (GraphQL-only).
  const currentFindings = new Set(inline.map(c => `${c.path}:${c.line}`));
  const fixedComments = existingComments.filter(c =>
    !c.in_reply_to_id &&
    /\[(blocking|important)\]/.test(c.body || '') &&
    !currentFindings.has(`${c.path}:${c.line}`)
  );
  const resolvedCount = await resolveFixedThreads(fixedComments);

  // Progress: context/read/CI/review done.
  await updateParentComment(progressBody(4, `**CI:** ${ciStatus}`));

  // Child inline comments are posted as a review (no body, like claude); the
  // parent comment is the fresh comment created above. A single hallucinated
  // line (a line number not part of the diff) 422s the whole call, so isolate it.
  if (newInline.length > 0) {
    try {
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
    } catch (e) {
      core.warning(`Inline comments failed (${newInline.length}): ${e.message}`);
    }
  }

  // Progress: inline comments posted.
  await updateParentComment(progressBody(5, `**CI:** ${ciStatus}`));

  const legend = 'Severity: **blocking** = must fix before merge · **important** = should fix in this PR · unlisted items were deferred or omitted as nits.';
  const loc = (c) => `\`${c.path}${typeof c.line === 'number' ? `:${c.line}` : ''}\``;
  const findingsLines = [
    ...newInline.map(c => `- [NEW] **${c.severity}** — ${loc(c)} — ${c.comment} — inline comment posted`),
    ...reflagged.map(c => {
      const prior = priorSeverity.get(`${c.path}:${c.line}`);
      const drift = prior && prior !== c.severity ? ` (prior round: ${prior})` : '';
      return `- [prior round] **${c.severity}**${drift} — ${loc(c)} — ${c.comment} — re-flagged from a prior round; not re-posted inline`;
    }),
  ];
  const summaryText = [
    `### DeepSeek AI Review — round update`,
    `**CI:** ${ciStatus}`,
    result.summary || '',
    result.readme_note ? `**README:** ${result.readme_note}` : '',
    result.architecture_note ? `**ARCHITECTURE:** ${result.architecture_note}` : '',
    `**Findings:** ${newInline.length} new · ${reflagged.length} re-flagged from prior rounds`,
    ...(findingsLines.length ? findingsLines : ['(no findings this round)']),
    ...(resolvedCount ? [`**Resolved:** ${resolvedCount} prior finding(s) — ${fixedComments.map(c => loc(c)).join(', ')}`] : []),
    legend,
    `**${result.verdict || 'NEEDS WORK: could not determine verdict'}**`,
  ].filter(Boolean).join('\n\n');

  const elapsed = Math.round((Date.now() - startedAt) / 1000);
  const runUrl = `${process.env.GITHUB_SERVER_URL}/${owner}/${repo}/actions/runs/${process.env.GITHUB_RUN_ID}`;
  const finalBody = [
    TRACKING_MARKER,
    `**DeepSeek finished @${pr.user.login}'s task in ${elapsed}s** — [View job](${runUrl})`,
    `---`,
    `### Review complete`,
    ...PROGRESS_ITEMS.map(p => `- [x] ${p}`),
    `---`,
    summaryText,
  ].join('\n');
  await updateParentComment(finalBody);

  core.info(`Posted ${newInline.length} inline comment(s). Verdict: ${result.verdict}`);

  // Fail the check (red) when the review reports blocking/important findings —
  // green only on MERGE-READY. Comments are already posted above.
  if (inline.length > 0 || /NEEDS WORK/i.test(result.verdict || '')) {
    core.setFailed(`Review found ${inline.length} blocking/important finding(s) (${newInline.length} new, ${reflagged.length} re-flagged) — see the parent comment.`);
  } else {
    core.info('Review passed — MERGE-READY.');
  }
}

main().catch(err => core.setFailed(err.message));
