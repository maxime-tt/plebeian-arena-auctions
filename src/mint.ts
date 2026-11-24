/**
 * The mint client: `GET /v1/info`, `GET /v1/keys`, `POST /v1/swap`.
 *
 * Deliberately thin and generic — it is not an arena mint client and it holds
 * no swap math (contract non-goal). All cryptography (B_, unblinding, DLEQ) is
 * cashu-ts's, used in `swap.ts`/`settle.ts`.
 */

import type { MintBlindSignature, SwapRequestBody, SwapResponseBody } from "./types.ts";

export interface MintDeps {
  fetchImpl: typeof fetch;
  timeoutMs: number;
}

export class MintError extends Error {
  override name = "MintError";
  constructor(
    message: string,
    readonly status: number,
    readonly code?: number,
  ) {
    super(message);
  }
}

export interface MintInfo {
  time?: number;
  [k: string]: unknown;
}

interface MintKeysetResponse {
  keysets: { id: string; unit?: string; active?: boolean; keys: Record<string, string> }[];
}

async function requestJson(
  deps: MintDeps,
  url: string,
  init: RequestInit,
): Promise<{ status: number; body: unknown; text: string }> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), deps.timeoutMs);
  let res: Response;
  try {
    res = await deps.fetchImpl(url, { ...init, signal: controller.signal });
  } catch (err) {
    throw new MintError(`mint request failed: ${(err as Error).message}`, 0);
  } finally {
    clearTimeout(timer);
  }
  const text = await res.text();
  let body: unknown = undefined;
  if (text !== "") {
    try {
      body = JSON.parse(text);
    } catch {
      body = undefined;
    }
  }
  if (!res.ok) {
    const detail =
      body && typeof body === "object" && "detail" in (body as Record<string, unknown>)
        ? String((body as Record<string, unknown>).detail)
        : text.slice(0, 200);
    const code =
      body && typeof body === "object" && typeof (body as Record<string, unknown>).code === "number"
        ? ((body as Record<string, unknown>).code as number)
        : undefined;
    throw new MintError(`mint ${res.status}: ${detail}`, res.status, code);
  }
  return { status: res.status, body, text };
}

/** `GET /v1/info`; `time` is the optional, non-standard clock extension. */
export async function getMintInfo(mintUrl: string, deps: MintDeps): Promise<MintInfo> {
  const { body } = await requestJson(deps, `${mintUrl}/v1/info`, { method: "GET" });
  if (body === null || typeof body !== "object") throw new MintError("mint /v1/info returned no object", 200);
  return body as MintInfo;
}

/**
 * `GET /v1/keys`: the keyset public keys (`amount → compressed pubkey hex`).
 * Selects `keysetId` when given, else the active keyset.
 */
export async function getMintKeys(
  mintUrl: string,
  keysetId: string | undefined,
  deps: MintDeps,
): Promise<Record<string, string>> {
  const { body } = await requestJson(deps, `${mintUrl}/v1/keys`, { method: "GET" });
  const resp = body as MintKeysetResponse;
  if (!resp || !Array.isArray(resp.keysets) || resp.keysets.length === 0) {
    throw new MintError("mint /v1/keys returned no keysets", 200);
  }
  const set = keysetId
    ? resp.keysets.find((k) => k.id === keysetId)
    : (resp.keysets.find((k) => k.active) ?? resp.keysets[0]);
  if (!set) throw new MintError(`mint has no keyset ${keysetId}`, 200);
  return set.keys;
}

/** `POST /v1/swap`. */
export async function postSwap(
  mintUrl: string,
  body: SwapRequestBody,
  deps: MintDeps,
): Promise<MintBlindSignature[]> {
  const { body: resp } = await requestJson(deps, `${mintUrl}/v1/swap`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
  const parsed = resp as SwapResponseBody;
  if (!parsed || !Array.isArray(parsed.signatures)) {
    throw new MintError("mint /v1/swap returned no signatures", 200);
  }
  return parsed.signatures;
}

/** NUT error 11001/11002 or an "already spent" detail means the input is spent. */
export function isAlreadySpent(err: unknown): boolean {
  if (!(err instanceof MintError)) return false;
  if (err.code === 11001 || err.code === 11002) return true;
  return /already spent|token already spent/i.test(err.message);
}
