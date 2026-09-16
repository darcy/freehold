import * as core from '@actions/core';
import * as github from '@actions/github';
import { readFileSync, existsSync } from 'fs';
import { reassembleStream } from './sse.mjs';

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

function readContextFiles() {
  // Context files are repo-root files (AGENTS.md etc.), but the job runs with
  // working-directory set to this script's dir — resolve them against the
  // workspace root, not cwd.
  const root = process.env.GITHUB_WORKSPACE || process.cwd();
  const files = CONTEXT_FILES
    .filter(f => existsSync(`${root}/${f}`))
    .map(f => {
      let content = readFileSync(`${root}/${f}`, 'utf8');
      if (content.length > MAX_CONTEXT_FILE_CHARS) {
        content = content.slice(0, MAX_CONTEXT_FILE_CHARS) + '\n...[truncated]';
      }
      return { name: f, text: `--- ${f} ---\n${content}` };
    });
  return { text: files.map(f => f.text).join('\n\n'), names: files.map(f => f.name) };
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

  return { ok: true, diff: annotatedFiles.map(f => f.text).join('\n\n'), fileCount: annotatedFiles.length, totalChars };
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

// Bound the reasoning budget: the flash reasoning model otherwise thinks for
// tens of thousands of tokens on a dense diff (~9 min/call). Capping
// `max_tokens` alone only truncates that thinking before it answers (prose,
// non-JSON); `reasoning_effort` makes it think less, so the JSON arrives fast.
const LLM_REASONING_EFFORT = process.env.LLM_REASONING_EFFORT ?? 'low';

async function callLlmOnce(prompt) {
  const res = await fetch(`${LLM_BASE_URL.replace(/\/$/, '')}/chat/completions`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${LLM_API_KEY}` },
    body: JSON.stringify({
      model: LLM_MODEL,
      temperature: 0.1,
      max_tokens: 16000,
      stream: true,
      ...(LLM_REASONING_EFFORT ? { reasoning_effort: LLM_REASONING_EFFORT } : {}),
      messages: [{ role: 'user', content: prompt }],
      tools: [REVIEW_TOOL],
      tool_choice: { type: 'function', function: { name: 'review' } },
    }),
  });
  if (!res.ok) throw new Error(`LLM API error ${res.status}: ${await res.text()}`);
  // Streamed: headers arrive immediately, so a multi-minute reasoning call no
  // longer trips undici's 5-minute headers timeout (which surfaced as
  // "fetch failed" and retried until the job looked hung).
  const msg = reassembleStream(await res.text());
  if (!msg) throw new Error('LLM returned no message');

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
// call, so retry once before failing the run. Retries are expensive on a large
// diff (a full reasoning call each), so keep the count low.
async function callLlm(prompt) {
  let lastErr = null;
  for (let attempt = 1; attempt <= 2; attempt++) {
    try {
      return await callLlmOnce(prompt);
    } catch (e) {
      lastErr = e;
      core.warning(`LLM attempt ${attempt}/2 failed: ${e.message}`);
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

// Resolve prior review-comment threads whose finding is no longer flagged this
// round (the finding was fixed). GraphQL-only; the token is sent explicitly.
async function resolveFixedThreads(fixedComments) {
  if (!fixedComments.length) return 0;
  const gh = (query, variables) => fetch('https://api.github.com/graphql', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `token ${GITHUB_TOKEN}` },
    body: JSON.stringify({ query, variables }),
  }).then(async (r) => {
    const body = await r.json();
    if (!r.ok || body.errors) throw new Error(JSON.stringify(body.errors || body));
    return body.data;
  });
  let data;
  try {
    data = await gh(`query($owner:String!,$repo:String!,$pr:Int!){
      repository(owner:$owner,name:$repo){ pullRequest(number:$pr){ reviewThreads(first:50){ nodes {
        id isResolved comments(first:20){ nodes { id databaseId } }
      } } } } }`, { owner, repo, pr: pull_number });
  } catch (e) {
    core.warning(`Could not read review threads: ${e.message}`);
    return 0;
  }
  const commentToThread = new Map();
  for (const t of data.repository.pullRequest.reviewThreads.nodes) {
    if (t.isResolved) continue;
    for (const c of t.comments.nodes) commentToThread.set(c.databaseId, t.id);
  }
  let resolved = 0;
  for (const c of fixedComments) {
    const threadId = commentToThread.get(c.id);
    if (!threadId) continue;
    try {
      await gh(`mutation($threadId:ID!){ resolveReviewThread(input:{threadId:$threadId}){ thread { id } } }`, { threadId });
      resolved += 1;
    } catch (e) {
      core.warning(`Could not resolve comment ${c.id}: ${e.message}`);
    }
  }
  return resolved;
}

const PROGRESS_ITEMS = [
  'Read the diff',
  'Gather context (AGENTS.md, README.md, ARCHITECTURE.md)',
  'Review for BLOCKING/IMPORTANT issues',
  'Post inline comments',
  'Post final summary with verdict',
];

// The parent comment is a fresh comment per commit; checkboxes start unchecked
// and get checked as the review progresses, with a running "Notes" list of what
// each step found (the diff size, the context read, the verdict, …).
function progressBody(doneCount, notes = []) {
  const checklist = PROGRESS_ITEMS.map((p, i) => `${i < doneCount ? '- [x]' : '- [ ]'} ${p}`);
  const parts = [TRACKING_MARKER, '### Bot Review', ...checklist];
  if (notes.length) parts.push('', '**Notes**', ...notes.map(n => `- ${n}`));
  return parts.join('\n');
}

const notes = [];
async function progress(doneCount, note) {
  if (note) notes.push(note);
  await updateParentComment(progressBody(doneCount, notes));
}

async function main() {
  const pr = github.context.payload.pull_request;
  const headSha = pr.head.sha;
  const startedAt = Date.now();

  // Create the progress comment up front so the checkboxes light up as work
  // happens, rather than appearing fully-formed at the end.
  await createParentComment(progressBody(0));

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
      `### Bot Review — skipped`,
      `**Reason:** ${isOversizedFile ? 'oversized file(s)' : 'diff too large'} ` +
        `(~${diffResult.totalChars.toLocaleString()} chars across ${diffResult.fileCount} file(s), limit ${MAX_DIFF_CHARS.toLocaleString()} chars).`,
      `${isOversizedFile ? 'Offending file(s)' : 'Largest files'}:\n${diffResult.offenders.map(f => `- ${f}`).join('\n')}`,
      guidance,
    ].join('\n\n');

    await updateParentComment(body);

    const message = `Diff too large to review reliably (${diffResult.reason}, ~${diffResult.totalChars} chars > ${MAX_DIFF_CHARS} limit).`;
    if (FAIL_ON_OVERSIZED_DIFF) {
      core.setFailed(message);
    } else {
      core.warning(message);
    }
    return;
  }

  const diff = diffResult.diff;
  await progress(1, `Read the diff — ${diffResult.fileCount} file(s), ~${diffResult.totalChars.toLocaleString()} chars`);

  const previousRound = await getPreviousRoundNotes();
  const ctx = readContextFiles();
  await progress(2, ctx.names.length ? `Read context — ${ctx.names.join(', ')}` : 'No context files found');

  const template = readFileSync(PROMPT_FILE, 'utf8');
  // Use function replacements: String.replace interprets $&, $', $$ etc. in the
  // replacement string, which corrupts the diff (the code contains '$&'), turning
  // it into '{{DIFF}}'. Functions avoid that substitution.
  const prompt = template
    .replace('{{REPO}}', () => `${owner}/${repo}`)
    .replace('{{PR_NUMBER}}', () => String(pull_number))
    .replace('{{PREVIOUS_ROUND}}', () => previousRound)
    .replace('{{CONTEXT_FILES}}', () => ctx.text)
    .replace('{{DIFF}}', () => diff);

  const result = await callLlm(prompt);

  // The weak flash model commonly stops after the first finding. Iterate ONCE:
  // ask again for ADDITIONAL distinct findings. Each pass is a full reasoning
  // call over the whole diff (minutes on a large PR), so the loop is capped at
  // one follow-up; first-round exhaustiveness is the model's job.
  const merged = (Array.isArray(result.inline) ? result.inline : []).slice();
  const already = () => new Set(merged.map(f => `${f.path}:${f.line}`));
  for (let pass = 1; pass <= 1 && merged.length > 0; pass++) {
    const foundText = merged.map(f => `- [${f.severity}] ${f.path}${typeof f.line === 'number' ? `:${f.line}` : ''}`).join('\n');
    const followUp = `PR ${owner}/${repo} #${pull_number}\n\nThese blocking/important findings are ALREADY reported:\n${foundText}\n\nReview the diff again. Report ONLY ADDITIONAL distinct blocking/important findings you have NOT already covered above — one per file:line. If there are no more, return an empty "inline" array and verdict "MERGE-READY".\n\nDo not repeat findings already listed.\n\nDIFF:\n${diff}`;
    let more;
    try {
      more = await callLlm(followUp);
    } catch (e) {
      core.warning(`Follow-up pass ${pass} failed: ${e.message}`);
      break;
    }
    const added = (Array.isArray(more.inline) ? more.inline : []).filter(f => !already().has(`${f.path}:${f.line}`));
    if (added.length === 0) break;
    merged.push(...added);
  }
  const inline = merged;
  await progress(3, `Reviewed — ${result.verdict}${inline.length ? ` · ${inline.length} finding(s)` : ''}`);

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

  // Post each NEW finding as its own review comment (thread) so every finding
  // shows up as a separate inline comment and a single bad/hallucinated line
  // 422s only that one, not the whole batch. Track how many actually posted.
  let postedInline = 0;
  if (newInline.length > 0) {
    for (const c of newInline) {
      try {
        await octokit.rest.pulls.createReview({
          owner, repo, pull_number,
          event: 'COMMENT',
          comments: [{ path: c.path, line: c.line, side: 'RIGHT', body: `**[${c.severity}]** ${c.comment}` }],
        });
        postedInline += 1;
      } catch (e) {
        core.warning(`Inline comment on ${c.path}:${c.line} failed: ${e.message}`);
      }
    }
  }

  await progress(4, `Posted ${postedInline} inline comment(s)` + (resolvedCount ? ` · resolved ${resolvedCount} prior thread(s)` : ''));

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
    `### Bot Review — round update`,
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
    `**Bot Review finished @${pr.user.login}'s task in ${elapsed}s** — [View job](${runUrl})`,
    `---`,
    `### Review complete`,
    ...PROGRESS_ITEMS.map(p => `- [x] ${p}`),
    `---`,
    summaryText,
  ].join('\n');
  await updateParentComment(finalBody);

  core.info(`Posted ${newInline.length} inline comment(s). Verdict: ${result.verdict}`);

  // Reflect the verdict as a real PR review state rather than a red check:
  // APPROVE when clean, REQUEST_CHANGES when blocking/important findings
  // remain. A later round's review supersedes the previous one (so a fixed PR
  // flips to APPROVE); the operator's override is dismissing the review.
  // Require a positive MERGE-READY signal — "not NEEDS WORK" would fail open on
  // an unexpected verdict.
  const clean = inline.length === 0 && /^MERGE-READY\b/i.test(result.verdict || '');
  // A PR author can't approve/request changes on their own PR; fall back to
  // COMMENT so the verdict still lands on PRs opened by the bot account.
  let event = clean ? 'APPROVE' : 'REQUEST_CHANGES';
  try {
    const { data: me } = await octokit.rest.users.getAuthenticated();
    if (me.login === pr.user.login) event = 'COMMENT';
  } catch { /* best-effort; createReview still guards */ }

  try {
    await octokit.rest.pulls.createReview({
      owner, repo, pull_number,
      event,
      // Empty body is allowed for APPROVE; REQUEST_CHANGES/COMMENT require a
      // non-empty one — send just the one-line verdict, since the detail already
      // lives in the parent comment and the inline findings.
      body: event === 'APPROVE' ? undefined : (result.verdict || 'NEEDS WORK: see the findings above'),
    });
    core.info(`Submitted ${event} review.`);
  } catch (e) {
    // The review state is the whole point of this block; a silent warning would
    // leave a stale REQUEST_CHANGES (or no state at all) behind a green check.
    core.setFailed(`Could not submit ${event} review: ${e.message}`);
  }
}

main().catch(err => core.setFailed(err.message));
