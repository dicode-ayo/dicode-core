// buildin/ai-agent-core — the shared suspend/resume "chat turn" envelope used
// by every ai-agent preset. Provider is the only difference between the tasks
// that build on this: each supplies a `runTurn` that executes one turn against
// its backend; opening the loop, ending it, and re-suspending for the next
// message all live here.
//
// `state` is provider-opaque: the envelope carries it through suspend/resume
// verbatim and hands it back to `runTurn`. For the Claude CLI it is the CLI
// session id + workdir key; for the OpenAI loop it is the cumulative
// {messages, summary}. The envelope never looks inside it.

import type { Dicode, JSONSchema, Output } from "../../sdk.ts";

// SAFE_SKILL_NAME caps skill filenames to a flat identifier so a `skills` param
// can't traverse out of skills_dir via "../" or absolute paths (the "/" is
// outside the class, so a traversal segment never matches). Mirrors the style
// of ValidateRunID elsewhere in the codebase.
export const SAFE_SKILL_NAME = /^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$/;

// SESSION_ID_RE caps chat/session identifiers to the UUID shape dicode itself
// mints via crypto.randomUUID() — and the shape the Claude CLI's own session
// ids take. Every place a chat/session id is carried through resume `state` or
// accepted as a param is attacker-influenceable (a resume submission, or a
// hand-crafted webhook body); once such a value reaches a subprocess arg
// (`claude --resume <id>`), a filesystem path component (the per-invocation
// workdir), or a KV/run-group key, an unvalidated string is a path-traversal /
// argument-injection vector. Reject anything off-shape rather than trying to
// escape it — the caller falls back to minting a fresh id.
export const SESSION_ID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function isValidSessionId(id: string): boolean {
    return SESSION_ID_RE.test(id);
}

// resolveSessionId centralizes the "validate a carried/param-supplied id;
// reject it if off-shape; optionally mint a fresh one" shape shared by every
// place a chat/session id enters (ai-agent-claude-cli's chatId + claudeSessionId,
// ai-agent's session_id). `autoMint: true` means absent-or-invalid becomes a
// fresh crypto.randomUUID() (used where the caller needs a stable handle
// regardless, e.g. a workdir key); `autoMint: false` means absent-or-invalid
// becomes "" (used where the caller should treat it as "no prior session",
// e.g. --resume — omitting the flag rather than resuming a hijacked one).
export function resolveSessionId(
    carried: string | undefined,
    label: string,
    opts: { autoMint: boolean },
): string {
    const id = carried ?? "";
    if (id && !isValidSessionId(id)) {
        console.warn(`${label}: rejected invalid session id; ${opts.autoMint ? "minting a fresh one" : "treating as absent"}`);
        return opts.autoMint ? crypto.randomUUID() : "";
    }
    if (!id && opts.autoMint) return crypto.randomUUID();
    return id;
}

// decideEntryMode picks the shape of a fresh run: a non-empty prompt runs one
// turn and returns; an empty prompt opens the interactive chat loop. Pure so the
// branch can be unit-tested without a live provider.
export function decideEntryMode(prompt: string): "one-shot" | "chat-start" {
    return prompt.trim() !== "" ? "one-shot" : "chat-start";
}

// isChatEnd terminates the chat loop: a blank/whitespace message is the "end"
// signal (the turn returns instead of suspending onward).
export function isChatEnd(message: string): boolean {
    return message.trim() === "";
}

// chatSchema is the resume form for one chat turn: a single `message` field,
// intentionally NOT required so the interactive CLI prompt accepts a blank line
// — which chatTurn reads as the end-of-chat signal (isChatEnd). The banner text
// lives on BOTH the schema and the `message` property — the CLI's interactive
// resume prompt renders the *property* description above the input
// (cmd/dicode/main.go promptResumeInput), while the no-field confirmation path
// renders the schema-level one; setting both keeps the reply visible regardless
// of which renderer runs.
export function chatSchema(description: string): JSONSchema {
    return {
        type: "object",
        title: "Chat",
        description,
        properties: {
            message: { type: "string", title: "Message", description },
        },
    };
}

// The outcome of one provider turn. Success carries the reply plus the new
// opaque state to carry into the next turn; failure is any object the provider
// wants surfaced as the run's result (chatTurn returns it verbatim).
export type TurnResult =
    | { ok: true; reply: string; state: unknown }
    | { ok: false; [k: string]: unknown };

// runTurn is the provider seam: run one turn for `message` given the carried
// `state`, returning the reply plus the state to carry forward.
export type RunTurn = (args: { message: string; state: unknown }) => Promise<TurnResult>;

// chatStart opens a fresh chat: suspend to the `turn` step with the seed state
// and a banner. Never returns — suspend() exits the process.
export function chatStart(dicode: Dicode, initialState: unknown, banner: string): Promise<never> {
    return dicode.suspend({
        to: "turn",
        state: initialState,
        schema: chatSchema(banner),
    });
}

// chatTurn runs one iteration of the loop: read the submitted message; a blank
// message ends the chat (onEnd), otherwise run the provider turn and suspend
// back to `turn` with the new state and the reply as the next banner. onEnd
// defaults to a bare end marker; providers that carry an identity in `state`
// (e.g. a session id) shape their own end result.
//
// A turn a provider could not run at all (`ok: false`) is terminal — nothing
// downstream (a webhook caller, WaitRunSettled, a chained trigger, the
// dashboard) should read it as a successful run carrying an error string in
// place of a reply. So this — not each provider — owns the terminality
// decision: publish the envelope via output.json (a webhook caller still
// receives it, over HTTP 500, because the daemon captures structured output
// before the non-zero exit) and throw, which is what makes the engine record
// a failed run. A provider that wants this behavior just returns
// `{ ok: false, ... }`; one that already throws its own terminal failures
// (e.g. ai-agent-claude-cli's fail()) never reaches this branch.
export async function chatTurn(
    ctx: { input: unknown; state?: unknown; dicode: Dicode; output: Output },
    runTurn: RunTurn,
    onEnd: (state: unknown) => unknown = () => ({ ok: true, reply: "(chat ended)" }),
): Promise<unknown> {
    const message = String((ctx.input as { message?: string } | undefined)?.message ?? "");
    if (isChatEnd(message)) return onEnd(ctx.state);

    const turn = await runTurn({ message, state: ctx.state });
    if (!turn.ok) {
        await ctx.output.json(turn);
        const reason = typeof turn.error === "string" ? turn.error : "chat turn failed";
        throw new Error(reason);
    }

    await ctx.dicode.suspend({
        to: "turn",
        state: turn.state,
        schema: chatSchema(turn.reply),
    });
    // unreachable — suspend() never returns
}
