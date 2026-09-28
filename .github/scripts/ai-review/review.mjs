import * as core from '@actions/core';
import * as github from '@actions/github';
import { readFileSync, existsSync, mkdtempSync, writeFileSync, rmSync } from 'fs';
import { spawn } from 'child_process';
import { createInterface } from 'readline';
import { tmpdir } from 'os';
import { join } from 'path';
import { parseReview, harnessText } from './parse.mjs';
import { buildReviewReplies, buildThreadIndex } from './threads.mjs';

const GITHUB_TOKEN = process.env.GITHUB_TOKEN;
const LLM_API_KEY = process.env.LLM_API_KEY;
const LLM_MODEL = process.env.LLM_MODEL;
// Wall-clock cap for the whole headless harness session (all of its turns), so
// a wedged run fails and retries instead of hanging to the job's 60-minute cap.
const HARNESS_TIMEOUT_MS = parseInt(process.env.HARNESS_TIMEOUT_MS || '1200000', 10);
const PROMPT_FILE = process.env.PROMPT_FILE || 'review-prompt.md';
const CONTEXT_FILES = (process.env.CONTEXT_FILES || '').split(',').map(s => s.trim()).filter(Boolean);
const EXCLUDE_PATTERNS = (process.env.EXCLUDE_PATTERNS || '').split(',').map(s => s.trim()).filter(Boolean);
const MAX_DIFF_CHARS = parseInt(process.env.MAX_DIFF_CHARS || '60000', 10);
const MAX_CONTEXT_FILE_CHARS = parseInt(process.env.MAX_CONTEXT_FILE_CHARS || '20000', 10);
const FAIL_ON_OVERSIZED_DIFF = (process.env.FAIL_ON_OVERSIZED_DIFF || 'true') === 'true';
const TRACKING_MARKER = '<!-- ai-review:tracking -->';

for (const [name, val] of Object.entries({ GITHUB_TOKEN, LLM_API_KEY, LLM_MODEL })) {
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
  // Paginate: a busy PR exceeds one page, and missing the prior tracking
  // comment would drop the re-review guidance entirely.
  const comments = await octokit.paginate(octokit.rest.issues.listComments, { owner, repo, issue_number: pull_number, per_page: 100 });
  // Each round creates a NEW parent comment, so take the most recent one that
  // is NOT this round's (defensive — normally we run before creating ours).
  const tracking = comments
    .filter(c => c.body?.includes(TRACKING_MARKER) && c.id !== parentCommentId)
    .sort((a, b) => new Date(b.created_at) - new Date(a.created_at))[0];
  return tracking ? tracking.body.replace(TRACKING_MARKER, '').trim() : '(first review round)';
}

// The bot's own login, fetched once. Used to tell the bot's comments apart from
// the author's replies and to filter review_requested events aimed elsewhere.
let botLogin = null;
async function getBotLogin() {
  if (botLogin !== null) return botLogin;
  try { botLogin = (await octokit.rest.users.getAuthenticated()).data.login; }
  catch { botLogin = ''; }
  return botLogin;
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

// The review runs as an opencode v2 headless session (`opencode run
// --standalone`) over the checked-out repo: it reads the attached instructions
// (repo context + full diff), explores the working tree with READ-ONLY tools
// to verify cross-file claims, and answers with the review JSON. The child env
// carries ONLY the model key — GITHUB_TOKEN never enters the harness, so a
// prompt-injected agent cannot touch the PR; this process keeps posting. The
// PR's own opencode config is ignored (OPENCODE_DISABLE_PROJECT_CONFIG) and
// OPENCODE_CONFIG_CONTENT — which merges after the project-level sources —
// carries the lockdown, so a shipped config cannot re-enable tools; --pure
// does not exist in v2, plugins are only loaded through config, and ours
// loads none. --standalone boots a private server (v2 otherwise attaches to
// a shared background service). Nothing from an unreviewed head is ever
// executed (the workflow relies on the same property).
const HARNESS_MODEL = LLM_MODEL.startsWith('openrouter/') ? LLM_MODEL : `openrouter/${LLM_MODEL}`;
const HARNESS_PERMISSIONS = JSON.stringify({
  $schema: 'https://opencode.ai/config.json',
  // v2 ordered rules, LAST MATCH WINS: deny everything, then allow only the
  // local discovery tools. The explicit dangerous-tool denies after the
  // wildcard are redundant with `*` but state the intent and survive any
  // `*`-semantics drift.
  permissions: [
    { action: '*', resource: '*', effect: 'deny' },
    { action: 'read', resource: '*', effect: 'allow' },
    { action: 'glob', resource: '*', effect: 'allow' },
    { action: 'grep', resource: '*', effect: 'allow' },
    { action: 'shell', resource: '*', effect: 'deny' },
    { action: 'edit', resource: '*', effect: 'deny' },
    { action: 'webfetch', resource: '*', effect: 'deny' },
    { action: 'websearch', resource: '*', effect: 'deny' },
    { action: 'subagent', resource: '*', effect: 'deny' },
    { action: 'skill', resource: '*', effect: 'deny' },
  ],
});

// The event stream is the debug surface: every line is logged as it arrives
// (tool calls especially), so a failed or slow review shows exactly what the
// agent explored. Shapes are handled leniently — v1 and v2 both spread the
// message part into the event, under `part`.
function summarizeEvent(e) {
  const p = e.part || {};
  const name = p.tool || p.name || e.tool || e.name || '';
  const brief = typeof p.description === 'string' && p.description
    ? p.description
    : JSON.stringify(p.metadata || p.arguments || p.input || '').slice(0, 120);
  return name ? `${name}: ${brief}` : JSON.stringify(e).slice(0, 140);
}

async function runHarnessOnce(prompt, timeoutMs) {
  // The prompt (context + diff) exceeds argv limits; attach it as a file.
  const dir = mkdtempSync(join(tmpdir(), 'ai-review-harness-'));
  try {
    const promptFile = join(dir, 'prompt.md');
    writeFileSync(promptFile, prompt);
    const message = 'You are a PR review agent. The attached file contains your complete ' +
      'review instructions, repo context, and the diff under review — follow it exactly. ' +
      'The repository is checked out at the current working directory at the PR state; ' +
      'use your read-only tools to inspect surrounding code and verify cross-file claims. ' +
      'Finish by outputting ONLY the single JSON review object the instructions specify.';
    const args = [
      'run', '--standalone', '--format', 'json', '--model', HARNESS_MODEL, '--file', promptFile, message,
    ];
    core.info(
      `Harness: opencode ${HARNESS_MODEL} · ${prompt.length.toLocaleString()} char prompt · ` +
      `timeout ${timeoutMs / 1000}s · OPENROUTER_API_KEY ${LLM_API_KEY ? 'set' : 'MISSING'}`,
    );
    const child = spawn('opencode', args, {
      cwd: process.env.GITHUB_WORKSPACE || process.cwd(),
      // stdin MUST be closed: the v2 CLI reads stdin to EOF before acting
      // (an open-but-empty pipe hangs it forever — spawnSync closed stdin
      // implicitly, async spawn does not).
      stdio: ['ignore', 'pipe', 'pipe'],
      // Node kills the child and reports the signal on close.
      timeout: timeoutMs,
      killSignal: 'SIGKILL',
      // Own process group: a timeout must reach descendants (the v1 runner's
      // known "timeout kills the shell, not its children" gap, closed here).
      detached: true,
      env: {
        PATH: process.env.PATH,
        HOME: process.env.HOME,
        TMPDIR: process.env.TMPDIR,
        OPENROUTER_API_KEY: LLM_API_KEY,
        OPENCODE_DISABLE_PROJECT_CONFIG: '1',
        OPENCODE_CONFIG_CONTENT: HARNESS_PERMISSIONS,
      },
    });
    // spawn's timeout kills the direct child only; the group kill happens on
    // the exit path (below) so a SIGTERM/SIGKILL can't leave a grandchild
    // holding the stdout pipe.
    let killed = false;
    child.on('close', (code, signal) => {
      if (signal && !killed) {
        killed = true;
        try { process.kill(-child.pid, 'SIGKILL'); } catch {}
      }
      // A lingering descendant can hold stdout open after close; stop
      // waiting for EOF shortly after the process is gone.
      setTimeout(() => { try { child.stdout.destroy(); child.stderr.destroy(); } catch {} }, 5000).unref();
    });

    const lines = [];
    const counts = {};
    let sessionError = null;
    const pumped = Promise.all([
      new Promise((resolve, reject) => {
        const rl = createInterface({ input: child.stdout });
        rl.on('line', (line) => {
          lines.push(line);
          let e;
          try { e = JSON.parse(line); } catch { return; }
          counts[e.type] = (counts[e.type] || 0) + 1;
          if (e.type === 'error') {
            // A failed session (bad key, provider outage, unknown model) is
            // reported as an `error` event and still exits 0 — surface it.
            sessionError = e.error?.data?.message || e.error?.message || e.error?.name || JSON.stringify(e.error).slice(0, 200);
            core.error(`Harness session error event: ${sessionError}`);
          } else if (e.type === 'tool_use' || e.type === 'step_start' || e.type === 'step_finish') {
            core.info(`  · ${summarizeEvent(e)}`);
          }
        });
        rl.on('error', reject);
        rl.on('close', resolve);
      }),
      new Promise((resolve) => { child.stderr.on('data', () => {}); child.stderr.on('close', resolve); }),
      new Promise((resolve, reject) => {
        child.on('close', (code, signal) => resolve({ code, signal }));
        child.on('error', reject);
      }),
    ]);

    const { code, signal } = await pumped;
    core.info(`Harness events: ${Object.entries(counts).map(([t, n]) => `${t}=${n}`).join(', ') || 'none'}`);
    if (signal) throw new Error(`opencode timed out after ${timeoutMs}ms (signal ${signal})`);
    if (sessionError) throw new Error(`opencode session error: ${sessionError}`);
    if (code !== 0) throw new Error(`opencode exited ${code}: ${lines.join('\n').slice(-500)}`);

    const text = harnessText(lines.join('\n')) || lines.join('\n');
    core.info(`Harness raw (${text.length} chars): ${text.trim().slice(0, 200)}`);
    core.info(`Harness raw tail: ${text.trim().slice(-300)}`);
    return parseReview(text);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

// HARNESS_TIMEOUT_MS is the TOTAL budget across attempts, so a timed-out
// session never gets a second full leash: attempt 1 takes 60%, the retry the
// remainder. A session that can't finish in either window is a problem run —
// fail the job rather than burn 40 minutes.
async function runHarness(prompt) {
  const deadline = Date.now() + HARNESS_TIMEOUT_MS;
  let lastErr = null;
  for (let attempt = 1; attempt <= 2; attempt++) {
    const remaining = deadline - Date.now();
    if (remaining <= 30_000) break;
    const budget = attempt === 1 ? Math.floor(HARNESS_TIMEOUT_MS * 0.6) : remaining;
    const startedAt = Date.now();
    try {
      return await runHarnessOnce(prompt, Math.min(budget, remaining));
    } catch (e) {
      lastErr = e;
      core.warning(`Harness attempt ${attempt}/2 failed after ${Math.round((Date.now() - startedAt) / 1000)}s: ${e.message}`);
    }
  }
  throw lastErr || new Error('harness budget exhausted');
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
  'Gather context (AGENTS.md, README.md, docs/ARCHITECTURE.md)',
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

  // Re-review is triggered by re-requesting a review. `review_requested` also
  // fires for human reviewers, so skip when the request is aimed at someone
  // other than the bot (the bot's own review does not emit this event).
  if (github.context.payload.action === 'review_requested') {
    const me = await getBotLogin();
    const requested = github.context.payload.requested_reviewer?.login;
    if (me && requested && requested !== me) {
      core.info(`Review requested from ${requested}, not the bot (${me}); skipping.`);
      return;
    }
  }

  // Read the PRIOR round's notes BEFORE creating this round's tracking comment:
  // getPreviousRoundNotes takes the most recent TRACKING_MARKER comment, so
  // creating ours first would make it read the fresh, empty one and drop the
  // re-review guidance ("don't re-find marginal issues") — which makes every
  // round reason like a first look (slow + prone to prose/non-JSON).
  const previousRound = await getPreviousRoundNotes();

  // Fetch the PR's review comments once: the author's replies feed the prompt,
  // the thread index lets a re-flagged finding be answered in-thread, and the
  // re-flag/fixed/resolve logic below reuses the same list.
  const bot = await getBotLogin();
  const existingComments = await octokit.paginate(octokit.rest.pulls.listReviewComments, { owner, repo, pull_number, per_page: 100 });
  const reviewReplies = buildReviewReplies(existingComments, { bot, author: pr.user.login });
  const threadIndex = buildThreadIndex(existingComments, { bot, author: pr.user.login });

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
    .replace('{{REVIEW_REPLIES}}', () => reviewReplies)
    .replace('{{CONTEXT_FILES}}', () => ctx.text)
    .replace('{{DIFF}}', () => diff);

  const result = await runHarness(prompt);

  // The harness explores iteratively by nature (it reads the repo rather than
  // answering from one prompt), so there is no follow-up machinery: whatever
  // it reports in `inline` is the round's finding set.
  const inline = Array.isArray(result.inline) ? result.inline : [];
  await progress(3, `Reviewed — ${result.verdict}${inline.length ? ` · ${inline.length} finding(s)` : ''}`);

  // Distinguish NEW findings (post as child inline comments) from RE-FLAGGED
  // findings (already commented in a prior round). GitHub rewrites a prior
  // comment's `commit_id` to the current head when its line persists, so
  // matching on `commit_id === headSha` reliably detects prior-round re-flags.
  // Key on both `line` and `original_line` (like buildThreadIndex) so a finding
  // whose line shifted is still recognised as a re-flag, not posted anew.
  // Thread roots only — replies share path:line and would confuse the match.
  const roots = existingComments.filter(c => !c.in_reply_to_id);
  const locKeys = (c) => [c.line, c.original_line]
    .filter(line => typeof line === 'number')
    .map(line => `${c.path}:${line}`);
  const seen = new Set(roots.filter(c => c.commit_id === headSha).flatMap(locKeys));
  const priorSeverity = new Map();
  for (const c of roots) {
    const m = c.body?.match(/\[(blocking|important)\]/) || [];
    if (m[1]) for (const k of locKeys(c)) priorSeverity.set(k, m[1]);
  }
  const newInline = [];
  const reflagged = [];
  for (const c of inline) {
    if (seen.has(`${c.path}:${c.line}`)) reflagged.push(c);
    else newInline.push(c);
  }

  // Prior blocking/important comments whose finding is no longer flagged this
  // round are treated as fixed — resolve their threads (GraphQL-only). Only the
  // bot's own threads are resolved (never a human reviewer's), and this is what
  // resolves a thread when the author's reply clarified the finding away and
  // the model dropped it.
  const currentFindings = new Set(inline.map(c => `${c.path}:${c.line}`));
  const fixedComments = roots.filter(c =>
    bot && c.user?.login === bot &&
    /\[(blocking|important)\]/.test(c.body || '') &&
    !locKeys(c).some(k => currentFindings.has(k))
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

  // A re-flagged finding whose thread the author replied to is answered
  // in-thread (the finding's `comment` is written as a direct response). With
  // no author reply there is nothing to answer, so it is only re-captured in
  // the parent summary — no self-reply.
  const repliedKeys = new Set();
  for (const c of reflagged) {
    const target = threadIndex.get(`${c.path}:${c.line}`);
    // Unknown bot identity → never reply on a thread we can't prove is ours.
    if (!bot || !target?.hasAuthorReply) continue;
    try {
      await octokit.rest.pulls.createReplyForReviewComment({
        owner, repo, pull_number,
        comment_id: target.rootId,
        body: `**[${c.severity}]** ${c.comment}`,
      });
      repliedKeys.add(`${c.path}:${c.line}`);
    } catch (e) {
      core.warning(`Thread reply on ${c.path}:${c.line} failed: ${e.message}`);
    }
  }
  const postedReplies = repliedKeys.size;

  await progress(4, `Posted ${postedInline} inline comment(s)` + (postedReplies ? ` · replied in ${postedReplies} thread(s)` : '') + (resolvedCount ? ` · resolved ${resolvedCount} prior thread(s)` : ''));

  const legend = 'Severity: **blocking** = must fix before merge · **important** = should fix in this PR · unlisted items were deferred or omitted as nits.';
  const loc = (c) => `\`${c.path}${typeof c.line === 'number' ? `:${c.line}` : ''}\``;
  const findingsLines = [
    ...newInline.map(c => `- [NEW] **${c.severity}** — ${loc(c)} — ${c.comment} — inline comment posted`),
    ...reflagged.map(c => {
      const prior = priorSeverity.get(`${c.path}:${c.line}`);
      const drift = prior && prior !== c.severity ? ` (prior round: ${prior})` : '';
      const how = repliedKeys.has(`${c.path}:${c.line}`)
        ? 'replied in-thread to the author'
        : 're-flagged from a prior round; not re-posted inline';
      return `- [prior round] **${c.severity}**${drift} — ${loc(c)} — ${c.comment} — ${how}`;
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
  if (bot && bot === pr.user.login) event = 'COMMENT';

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
