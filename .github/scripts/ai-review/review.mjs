import * as core from '@actions/core';
import * as github from '@actions/github';
import { readFileSync, existsSync, mkdtempSync, writeFileSync, rmSync } from 'fs';
import { spawn } from 'child_process';
import { createInterface } from 'readline';
import { tmpdir } from 'os';
import { join } from 'path';
import { parseReviewFromEvents, harnessTextParts } from './parse.mjs';
import {
  buildReviewReplies,
  botFindingRoots,
  buildOpenThreads,
  stillOpen,
  openThreadsPrompt,
  classifyFindings,
  renderFindingsTable,
  renderResolved,
} from './threads.mjs';

const GITHUB_TOKEN = process.env.GITHUB_TOKEN;
// HARNESS_API_KEY/HARNESS_MODEL take precedence over the legacy single-shot
// vars (LLM_API_KEY/LLM_MODEL) — during the transition both sets are present
// because the job may run main's previous script until this merges.
const LLM_API_KEY = process.env.HARNESS_API_KEY || process.env.LLM_API_KEY;
const LLM_MODEL = process.env.HARNESS_MODEL || process.env.LLM_MODEL;
// Wall-clock cap for the whole headless harness session (all of its turns), so
// a wedged run fails and retries instead of hanging to the job's 60-minute cap.
// Parsed with validation: a malformed operator value must fail the run, not
// silently disable the cap (NaN propagates through spawn's timeout as "no
// timeout"; parseInt('15m') yields 15 — a 15ms cap).
const HARNESS_TIMEOUT_MS = parseInt(process.env.HARNESS_TIMEOUT_MS || '1200000', 10);
if (!Number.isFinite(HARNESS_TIMEOUT_MS) || HARNESS_TIMEOUT_MS < 60_000) {
  throw new Error(`HARNESS_TIMEOUT_MS must be a millisecond value >= 60000 (got ${process.env.HARNESS_TIMEOUT_MS})`);
}
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
  // Context files are repo-root files, read from the TRUSTED checkout this
  // script lives in — this dir is <root>/.github/scripts/ai-review, so the
  // repo root is THREE levels up. Never resolve against GITHUB_WORKSPACE,
  // which is the explored PR state (untrusted data).
  const root = join(import.meta.dirname, '../../..');
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

// A new review round invalidates the previous one: dismiss the bot's own
// outstanding verdict (APPROVED / CHANGES_REQUESTED) so a stale approval from
// an earlier revision can never satisfy branch protection — including
// re-request-triggered rounds, which push no commits for GitHub's stale-
// approval auto-dismiss to catch. COMMENTED reviews gate nothing; leave them.
async function dismissOwnPriorReviews() {
  const me = await getBotLogin();
  if (!me) return 0;
  const reviews = await octokit.paginate(octokit.rest.pulls.listReviews, { owner, repo, pull_number, per_page: 100 });
  const mine = reviews.filter(r => r.user?.login === me && (r.state === 'APPROVED' || r.state === 'CHANGES_REQUESTED'));
  for (const r of mine) {
    try {
      await octokit.rest.pulls.dismissReview({
        owner, repo, pull_number, review_id: r.id,
        message: 'Superseded: a new review round is starting; this verdict applied to a previous revision.',
      });
      core.info(`Dismissed my prior ${r.state.toLowerCase()} review (id ${r.id}).`);
    } catch (e) {
      // Already dismissed (or a rights blip) must not kill the round — the
      // fresh verdict below replaces it either way.
      core.warning(`Could not dismiss prior review ${r.id}: ${e.message}`);
    }
  }
  return mine.length;
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
// LLM_MODEL is the FULL opencode model id: `<provider>/<model>` — e.g.
// `openrouter/deepseek/deepseek-v4.1-flash` or
// `fireworks-ai/accounts/fireworks/models/glm-5p3-flash`. LLM_KEY_ENV names
// the provider's expected key env var (OPENROUTER_API_KEY, FIREWORKS_API_KEY,
// ...) and LLM_API_KEY holds the key itself; only that env var enters the
// harness process.
const HARNESS_MODEL = LLM_MODEL;
const LLM_KEY_ENV = process.env.LLM_KEY_ENV || 'OPENROUTER_API_KEY';
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

// Actions interprets %-sequences and ::-prefixed lines in log output as
// workflow commands (::add-mask::, ::stop-commands::, ::error:: …). Harness
// output is attacker-influenced (the PR controls the diff and the files the
// agent reads), so every untrusted string must be escaped before logging.
const logSafe = s => String(s).replace(/%/g, '%25').replace(/\r/g, '%0D').replace(/\n/g, '%0A');

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
  return logSafe(name ? `${name}: ${brief}` : JSON.stringify(e).slice(0, 140));
}

async function runHarnessOnce(prompt, timeoutMs) {
  // The prompt (context + diff) exceeds argv limits; attach it as a file.
  const dir = mkdtempSync(join(tmpdir(), 'ai-review-harness-'));
  try {
    const promptFile = join(dir, 'prompt.md');
    writeFileSync(promptFile, prompt);
    const message = 'You are a PR review agent. The attached file contains your complete ' +
      'review instructions, repo context, and the diff under review — follow it exactly. ' +
      'The repository is checked out at the current working directory (the base branch — ' +
      'trusted main, the same tree the harness itself runs from) — the attached diff, not ' +
      'the working tree, is the source of truth for what changed. ' +
      'Use your read-only tools to inspect surrounding code and verify cross-file claims. ' +
      'Finish by outputting ONLY the single JSON review object the instructions specify.';
    const args = [
      'run', '--standalone', '--format', 'json', '--model', HARNESS_MODEL, '--file', promptFile, message,
    ];
    core.info(
      `Harness: opencode ${HARNESS_MODEL} · ${prompt.length.toLocaleString()} char prompt · ` +
      `timeout ${timeoutMs / 1000}s · ${LLM_KEY_ENV} ${LLM_API_KEY ? 'set' : 'MISSING'}`,
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
        [LLM_KEY_ENV]: LLM_API_KEY,
        OPENCODE_DISABLE_PROJECT_CONFIG: '1',
        OPENCODE_CONFIG_CONTENT: HARNESS_PERMISSIONS,
      },
    });
    const lines = [];
    const counts = {};
    let sessionError = null;
    const rl = createInterface({ input: child.stdout });
    // The group kill and the stream shutdown ride 'exit' — it fires the moment
    // the process dies, while 'close' waits for stdout/stderr EOF and a
    // lingering descendant holding the pipes suppresses it forever (the run
    // would hang to the job's 60-minute cap instead of failing into the
    // retry). spawn's timeout kills the direct child only; the group kill
    // here is what reaches grandchildren.
    let killed = false;
    child.on('exit', (code, signal) => {
      if (signal && !killed) {
        killed = true;
        try { process.kill(-child.pid, 'SIGKILL'); } catch {}
      }
      // Stop waiting for EOF shortly after the process is gone: a descendant
      // can hold stdout/stderr open past exit. rl.close() specifically —
      // destroying the stream alone does not close a readline that never saw
      // EOF (node readline does not forward a destroyed input's close).
      setTimeout(() => { try { rl.close(); child.stdout.destroy(); child.stderr.destroy(); } catch {} }, 5000).unref();
    });

    const pumped = Promise.all([
      new Promise((resolve, reject) => {
        rl.on('line', (line) => {
          lines.push(line);
          let e;
          try { e = JSON.parse(line); } catch { return; }
          counts[e.type] = (counts[e.type] || 0) + 1;
          if (e.type === 'error') {
            sessionError = e.error?.data?.message || e.error?.message || e.error?.name;
            if (!sessionError) {
              // The last-ditch fallback must not throw: JSON.stringify(undefined)
              // yields undefined (and throws on cycles), and a throw inside this
              // readline listener is an uncaught exception that kills the run —
              // the opposite of what this branch exists for.
              try { sessionError = JSON.stringify(e.error ?? null).slice(0, 200); }
              catch { sessionError = String(e.error).slice(0, 200); }
            }
            core.error(`Harness session error event: ${logSafe(sessionError)}`);
          } else if (e.type === 'tool_use' || e.type === 'step_start' || e.type === 'step_finish') {
            core.info(`  · ${summarizeEvent(e)}`);
          }
        });
        rl.on('error', reject);
        rl.on('close', resolve);
      }),
      new Promise((resolve) => { child.stderr.on('data', () => {}); child.stderr.on('close', resolve); }),
      new Promise((resolve, reject) => {
        // 'exit', not 'close': close can be suppressed by a descendant
        // holding the stdio pipes; exit always fires, and the 5s destroy
        // (above) bounds the stream waiters this promise is combined with.
        child.on('exit', (code, signal) => resolve({ code, signal }));
        child.on('error', reject);
      }),
    ]);

    // Promise.all resolves to [stdoutResult, stderrResult, exitStatus] — the
    // exit status is the THIRD element; the first two resolve with no value.
    const { code, signal } = (await pumped)[2];
    core.info(`Harness events: ${Object.entries(counts).map(([t, n]) => `${t}=${n}`).join(', ') || 'none'}`);
    if (signal) throw new Error(`opencode timed out after ${timeoutMs}ms (signal ${signal})`);
    if (sessionError) throw new Error(`opencode session error: ${sessionError}`);
    if (code !== 0) throw new Error(`opencode exited ${code}: ${logSafe(lines.join('\n').slice(-500))}`);

    // The model's final answer is its LAST text part (the prompt requires the
    // review JSON alone there). parseReviewFromEvents is the production parse
    // path — only that part reaches the verdict extractor; earlier parts and
    // the raw event stream (tool-result events carry attacker-controlled file
    // contents) never do. No fallback scan: a format drift must fail loudly
    // (and retry), not parse attacker text.
    const stdout = lines.join('\n');
    const finalPart = harnessTextParts(stdout).pop();
    if (finalPart) {
      core.info(`Harness answer (${finalPart.length} chars): ${logSafe(finalPart.trim().slice(0, 200))}`);
      core.info(`Harness answer tail: ${logSafe(finalPart.trim().slice(-300))}`);
    }
    return parseReviewFromEvents(stdout);  } finally {
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
      // e.message embeds untrusted content (session errors, raw model text) —
      // escape it or a crafted failure can emit workflow commands.
      core.warning(`Harness attempt ${attempt}/2 failed after ${Math.round((Date.now() - startedAt) / 1000)}s: ${logSafe(e.message)}`);
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

// GraphQL-only review-thread state (REST cannot see thread resolution). Maps
// every thread-root comment id to { threadId, isResolved }; null when the
// query fails — callers then treat every tagged root as still open (the
// conservative side: nothing gets resolved, open threads stay listed).
async function graphql(query, variables) {
  const r = await fetch('https://api.github.com/graphql', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `token ${GITHUB_TOKEN}` },
    body: JSON.stringify({ query, variables }),
  });
  const body = await r.json();
  if (!r.ok || body.errors) throw new Error(JSON.stringify(body.errors || body));
  return body.data;
}

async function fetchThreadState() {
  try {
    const data = await graphql(`query($owner:String!,$repo:String!,$pr:Int!){
      repository(owner:$owner,name:$repo){ pullRequest(number:$pr){ reviewThreads(first:100){ nodes {
        id isResolved comments(first:20){ nodes { id databaseId } }
      } } } } }`, { owner, repo, pr: pull_number });
    const map = new Map();
    for (const t of data.repository.pullRequest.reviewThreads.nodes) {
      for (const c of t.comments.nodes) map.set(c.databaseId, { threadId: t.id, isResolved: t.isResolved });
    }
    return map;
  } catch (e) {
    core.warning(`Could not read review threads: ${e.message}`);
    return null;
  }
}

// Resolve the given { threadId } entries; returns the ones that resolved (so
// the round's "Resolved this round" list only shows what actually closed).
async function resolveThreads(entries) {
  const done = [];
  for (const entry of entries) {
    try {
      await graphql(`mutation($threadId:ID!){ resolveReviewThread(input:{threadId:$threadId}){ thread { id } } }`, { threadId: entry.threadId });
      done.push(entry);
    } catch (e) {
      core.warning(`Could not resolve thread ${entry.threadId}: ${e.message}`);
    }
  }
  return done;
}

const PROGRESS_ITEMS = [
  'Dismiss previous review',
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

  // FIRST step of every round: retire the bot's own prior verdict, so the
  // only review that can gate a merge is the one this run produces.
  await dismissOwnPriorReviews();

  // Read the PRIOR round's notes BEFORE creating this round's tracking comment:
  // getPreviousRoundNotes takes the most recent TRACKING_MARKER comment, so
  // creating ours first would make it read the fresh, empty one and drop the
  // re-review guidance ("don't re-find marginal issues") — which makes every
  // round reason like a first look (slow + prone to prose/non-JSON).
  const previousRound = await getPreviousRoundNotes();

  // Fetch the PR's review comments once: the author's replies feed the prompt,
  // the open-thread set drives classification/resolution, and the re-flag
  // replies below reuse the same list.
  const bot = await getBotLogin();
  const existingComments = await octokit.paginate(octokit.rest.pulls.listReviewComments, { owner, repo, pull_number, per_page: 100 });
  const reviewReplies = buildReviewReplies(existingComments, { bot, author: pr.user.login });

  // The bot's still-open prior threads: severity-tagged roots that GraphQL does
  // NOT mark resolved. These are what the model may re-flag, and what counts as
  // "fixed this round" when the model drops them. Without thread state (query
  // failed) every tagged root counts as open — resolve nothing, list everything.
  const threadState = await fetchThreadState();
  const openRoots = stillOpen(botFindingRoots(existingComments, { bot }), threadState);
  const openThreads = buildOpenThreads(openRoots);

  // Create the progress comment up front so the checkboxes light up as work
  // happens, rather than appearing fully-formed at the end. The dismissal
  // step is already done, so its box starts checked.
  await createParentComment(progressBody(1));

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
  await progress(2, `Read the diff — ${diffResult.fileCount} file(s), ~${diffResult.totalChars.toLocaleString()} chars`);

  const ctx = readContextFiles();
  await progress(3, ctx.names.length ? `Read context — ${ctx.names.join(', ')}` : 'No context files found');

  const template = readFileSync(PROMPT_FILE, 'utf8');
  // Use function replacements: String.replace interprets $&, $', $$ etc. in the
  // replacement string, which corrupts the diff (the code contains '$&'), turning
  // it into '{{DIFF}}'. Functions avoid that substitution.
  const prompt = template
    .replace('{{REPO}}', () => `${owner}/${repo}`)
    .replace('{{PR_NUMBER}}', () => String(pull_number))
    .replace('{{PREVIOUS_ROUND}}', () => previousRound)
    .replace('{{OPEN_THREADS}}', () => openThreadsPrompt(openThreads))
    .replace('{{REVIEW_REPLIES}}', () => reviewReplies)
    .replace('{{CONTEXT_FILES}}', () => ctx.text)
    .replace('{{DIFF}}', () => diff);

  const result = await runHarness(prompt);

  // result.inline is the round's finding set, normalized by the parser
  // (severity coerced, junk entries dropped). Classification: an exact
  // location hit on an open thread is a re-flag regardless of the model's
  // prior flag; the prior flag catches line drift; everything else is fresh.
  const inline = result.inline;
  await progress(4, `Reviewed — ${result.verdict}${inline.length ? ` · ${inline.length} finding(s)` : ''}`);

  const { fresh, reflagged } = classifyFindings(inline, openThreads);

  // Threads still open but no longer flagged this round are fixed — resolve
  // them (GraphQL-only). Only the bot's own open threads are candidates, and
  // only those the thread state proves are unresolved: without the bot's
  // identity the roots can't be provenance-checked, so nothing resolves (a
  // human reviewer's tagged thread must never be auto-resolved by the bot).
  const attachedRootIds = new Set(reflagged.map(c => c.thread.rootId));
  const fixedRoots = openRoots.filter(c => !attachedRootIds.has(c.id));
  const resolveEntries = bot && threadState
    ? fixedRoots.flatMap((c) => {
        const st = threadState.get(c.id);
        return st && !st.isResolved ? [{ threadId: st.threadId, root: c }] : [];
      })
    : [];
  const resolved = await resolveThreads(resolveEntries);
  const resolvedThreads = buildOpenThreads(resolved.map(e => e.root));

  // Post each NEW blocking/important finding as its own review comment (thread)
  // so every finding shows up as a separate inline comment and a single
  // bad/hallucinated line 422s only that one, not the whole batch. Suggestions
  // never become threads — they render in the parent table only.
  let postedInline = 0;
  const newInline = fresh.filter(c => c.severity !== 'suggestion');
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

  // Every re-flagged finding is answered on its existing thread — a persistent
  // finding keeps ONE discussion instead of growing a duplicate thread per
  // round (the path:line shift used to re-post it as new).
  let postedReplies = 0;
  for (const c of reflagged) {
    // Unknown bot identity → never reply on a thread we can't prove is ours.
    if (!bot) continue;
    try {
      await octokit.rest.pulls.createReplyForReviewComment({
        owner, repo, pull_number,
        comment_id: c.thread.rootId,
        body: `**[${c.severity}]** ${c.comment}`,
      });
      postedReplies += 1;
    } catch (e) {
      core.warning(`Thread reply on ${c.path}:${c.line} failed: ${e.message}`);
    }
  }

  await progress(5, `Posted ${postedInline} inline comment(s)` + (postedReplies ? ` · replied in ${postedReplies} thread(s)` : '') + (resolved.length ? ` · resolved ${resolved.length} prior thread(s)` : ''));

  // Permalinks for this round's freshly posted comments: anything with an id
  // above the pre-round max is ours (commit_id is unreliable — GitHub rewrites
  // it when a line persists). Re-flagged rows link their thread root instead.
  const maxPriorId = existingComments.reduce((m, c) => Math.max(m, c.id), 0);
  const discussionUrls = new Map();
  for (const c of await octokit.paginate(octokit.rest.pulls.listReviewComments, { owner, repo, pull_number, per_page: 100 })) {
    if (c.id > maxPriorId) discussionUrls.set(`${c.path}:${c.line}`, c.id);
  }

  // The verdict line: word from the model (fail closed — an unparseable verdict
  // can never read MERGE-READY), counts computed from the round's actual
  // findings, never trusted from model text.
  const blockingCount = inline.filter(c => c.severity === 'blocking').length;
  const importantCount = inline.filter(c => c.severity === 'important').length;
  const suggestionCount = inline.filter(c => c.severity === 'suggestion').length;
  const mergeReady = /^merge-ready\b/i.test(result.verdict || '') && blockingCount + importantCount === 0;
  const verdictLine = mergeReady ? 'MERGE-READY' : `NEEDS WORK — ${blockingCount} blocking, ${importantCount} important`;

  const freshBI = newInline.length;
  const findingsLine = `**Findings:** ${freshBI} new · ${reflagged.length} re-flagged` +
    (suggestionCount ? ` · ${suggestionCount} suggestion${suggestionCount === 1 ? '' : 's'}` : '');
  const tableRows = renderFindingsTable([...reflagged, ...fresh], {
    owner, repo, prNumber: pull_number, headSha, discussionUrls,
  });
  const table = tableRows.length
    ? ['| | Severity | Location | Finding |', '|---|----------|----------|---------|', ...tableRows].join('\n')
    : '(no findings this round)';

  const elapsed = Math.round((Date.now() - startedAt) / 1000);
  const runUrl = `${process.env.GITHUB_SERVER_URL}/${owner}/${repo}/actions/runs/${process.env.GITHUB_RUN_ID}`;
  const finalBody = [
    TRACKING_MARKER,
    `**Bot Review finished @${pr.user.login}'s task in ${elapsed}s** — [View job](${runUrl})`,
    `---`,
    `### Verdict: **${verdictLine}**`,
    result.summary || '',
    findingsLine,
    table,
    ...renderResolved(resolvedThreads, { owner, repo, prNumber: pull_number }),
    `🛑 must fix · ⚠️ should fix · 💡 suggestion · ✅ resolved`,
  ].filter(Boolean).join('\n\n');
  await updateParentComment(finalBody);

  core.info(`Posted ${postedInline} inline comment(s). Verdict: ${verdictLine}`);

  // Reflect the verdict as a real PR review state rather than a red check:
  // APPROVE only on a positive MERGE-READY signal AND zero blocking/important
  // findings (suggestions never block); REQUEST_CHANGES otherwise. A later
  // round's review supersedes the previous one; the operator's override is
  // dismissing the review.
  const clean = mergeReady;
  // A PR author can't approve/request changes on their own PR; fall back to
  // COMMENT so the verdict still lands on PRs opened by the bot account.
  let event = clean ? 'APPROVE' : 'REQUEST_CHANGES';
  if (bot && bot === pr.user.login) event = 'COMMENT';

  try {
    await octokit.rest.pulls.createReview({
      owner, repo, pull_number,
      event,
      // One-line verdict — the detail lives in the parent comment and the
      // inline findings.
      body: `${verdictLine} — details in the Bot Review comment`,
    });
    core.info(`Submitted ${event} review.`);
  } catch (e) {
    // The review state is the whole point of this block; a silent warning would
    // leave a stale REQUEST_CHANGES (or no state at all) behind a green check.
    core.setFailed(`Could not submit ${event} review: ${e.message}`);
  }
}

main().catch(err => core.setFailed(err.message));
