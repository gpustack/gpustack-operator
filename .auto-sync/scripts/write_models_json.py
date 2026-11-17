#!/usr/bin/env python3
"""Resolve organization LLM gateway environment variables into a Pi models.json file."""

from __future__ import annotations

import argparse
import json
import math
import os
import re
import sys
import urllib.parse

ENV_NAME_RE = re.compile(r"^[A-Z][A-Z0-9_]*$")
PROVIDER_ID_RE = re.compile(r"^[a-z0-9][a-z0-9._-]{0,63}$")

PI_WIRE_APIS = ("openai-completions", "openai-responses", "anthropic-messages")
PI_THINKING_LEVELS = ("off", "minimal", "low", "medium", "high", "xhigh", "max")
PI_OAUTH_ONLY_PROVIDERS = frozenset(
    {
        "github-copilot",
        "openai-codex",
        "claude-code",
        "gemini",
        "google",
        "gemini-cli",
        "google-gemini-cli",
        "google-antigravity",
    }
)


def first_env(*names: str) -> str:
    for name in names:
        val = os.environ.get(name, "").strip()
        if val:
            return val
    return ""


def parse_bool_env(raw: str) -> bool:
    val = raw.strip().lower()
    if val in ("1", "true", "yes", "on"):
        return True
    if val in ("0", "false", "no", "off", ""):
        return False
    raise ValueError(f"invalid boolean value: {raw!r}")


def normalize_and_validate_url(raw_url: str) -> str:
    if any(ch.isspace() or ord(ch) < 32 for ch in raw_url):
        raise ValueError("LLM URL contains whitespace or control characters")
    parsed = urllib.parse.urlsplit(raw_url)
    if not parsed.scheme or not parsed.netloc or not parsed.hostname:
        raise ValueError(f"invalid LLM URL: {raw_url}")
    if parsed.username is not None or parsed.password is not None:
        raise ValueError("LLM URL must not contain embedded credentials")
    if parsed.query or parsed.fragment:
        raise ValueError("LLM URL must not contain query parameters or fragments")

    scheme = parsed.scheme.lower()
    host = parsed.hostname.lower()
    port = parsed.port
    if port is not None and port == 0:
        raise ValueError("invalid LLM URL port: 0")
    if scheme == "https":
        pass
    elif scheme == "http":
        if host not in ("127.0.0.1", "::1") or port is None:
            raise ValueError(
                "LLM URL must use https:// (or explicit-port http://127.0.0.1:<port> / http://[::1]:<port>)"
            )
    else:
        raise ValueError(f"unsupported LLM URL scheme: {parsed.scheme}")

    path = parsed.path.rstrip("/")
    for suffix in ("/chat/completions", "/messages", "/responses"):
        if path.endswith(suffix):
            path = path[: -len(suffix)].rstrip("/")
            break

    return urllib.parse.urlunsplit((scheme, parsed.netloc, path, "", ""))


def infer_provider(base_url: str, explicit_provider: str) -> str:
    if explicit_provider:
        provider = explicit_provider.strip().lower()
    else:
        parsed = urllib.parse.urlsplit(base_url)
        host = (parsed.hostname or "").lower()
        path = parsed.path.lower()
        if host == "open.bigmodel.cn" or host.endswith(".bigmodel.cn"):
            if "/coding" in path:
                provider = "zai-coding-cn"
            else:
                provider = "zai-cn"
        elif host == "api.z.ai" or host.endswith(".z.ai"):
            provider = "zai"
        elif host == "api.anthropic.com" or host.endswith(".anthropic.com"):
            provider = "anthropic"
        elif host == "api.openai.com" or host.endswith(".openai.com"):
            provider = "openai"
        elif host == "api.deepseek.com" or host.endswith(".deepseek.com"):
            provider = "deepseek"
        elif host == "openrouter.ai" or host.endswith(".openrouter.ai"):
            provider = "openrouter"
        elif host == "api.moonshot.cn" or host.endswith(".moonshot.cn"):
            provider = "moonshotai-cn"
        elif host == "api.moonshot.ai" or host.endswith(".moonshot.ai"):
            provider = "moonshotai"
        elif host == "api.kimi.com" or host.endswith(".kimi.com"):
            provider = "kimi-coding"
        elif host.endswith(".minimaxi.com") or host == "api.minimaxi.com" or host.endswith(".minimax.chat"):
            provider = "minimax-cn"
        elif host == "api.minimax.io" or host.endswith(".minimax.io"):
            provider = "minimax"
        elif host == "dashscope.aliyuncs.com" or host.endswith(".dashscope.aliyuncs.com"):
            provider = "qwen-token-plan-cn"
        elif host == "dashscope-intl.aliyuncs.com" or host.endswith(".dashscope-intl.aliyuncs.com"):
            provider = "qwen-token-plan"
        else:
            provider = "custom-gateway"

    if not PROVIDER_ID_RE.fullmatch(provider) or ".." in provider:
        raise ValueError(f"invalid Pi provider identifier: {provider!r}")
    if provider in PI_OAUTH_ONLY_PROVIDERS:
        raise ValueError(
            f"OAuth-only Pi provider {provider!r} is unsupported; use a token-authenticated provider"
        )
    return provider


def resolve_wire_api(protocol: str, use_anthropic: bool, provider: str) -> str:
    if protocol:
        norm = protocol.strip().lower()
        if norm not in PI_WIRE_APIS:
            raise ValueError(
                f"unsupported protocol {protocol!r}; expected one of {', '.join(PI_WIRE_APIS)}"
            )
        return norm
    if use_anthropic or provider == "anthropic":
        return "anthropic-messages"
    return "openai-completions"


def resolve_thinking(raw_thinking: str, raw_effort: str) -> str:
    if raw_thinking:
        val = raw_thinking.strip().lower()
    elif raw_effort:
        val = raw_effort.strip().lower()
        if val == "none":
            val = "off"
    else:
        val = "high"

    if val not in PI_THINKING_LEVELS:
        raise ValueError(
            f"unsupported thinking level {val!r}; expected one of {', '.join(PI_THINKING_LEVELS)}"
        )
    return val


def parse_float_param(raw: str, name: str, lo: float, hi: float) -> float | None:
    if not raw:
        return None
    try:
        val = float(raw)
    except ValueError as exc:
        raise ValueError(f"{name} must be a number, got {raw!r}") from exc
    if not math.isfinite(val) or val < lo or val > hi:
        raise ValueError(f"{name} must be within [{lo}, {hi}], got {val}")
    if val == 0.0:
        val = 0.0
    return val


def parse_int_param(raw: str, name: str, lo: int, hi: int) -> int | None:
    if not raw:
        return None
    try:
        val = int(raw, 10)
    except ValueError as exc:
        raise ValueError(f"{name} must be an integer, got {raw!r}") from exc
    if val < lo or val > hi:
        raise ValueError(f"{name} must be within [{lo}, {hi}], got {val}")
    return val


def write_github_outputs(path: str, pairs: list[tuple[str, str]]) -> None:
    if not path:
        return
    for key, val in pairs:
        if "\n" in val or "\r" in val:
            raise ValueError(f"output value for {key!r} must be single-line")
    abs_path = os.path.abspath(path)
    if os.path.islink(abs_path):
        raise ValueError(f"refusing symlink output path: {abs_path}")
    parent = os.path.dirname(abs_path)
    if parent:
        os.makedirs(parent, exist_ok=True)
    flags = os.O_WRONLY | os.O_CREAT | os.O_APPEND | getattr(os, "O_NOFOLLOW", 0)
    fd = os.open(abs_path, flags, 0o600)
    with os.fdopen(fd, "a", encoding="utf-8") as f:
        for key, val in pairs:
            f.write(f"{key}={val}\n")


def write_models_file(path: str, payload: dict[str, object]) -> str:
    abs_path = os.path.abspath(path)
    if os.path.islink(abs_path):
        raise ValueError(f"refusing symlink models output path: {abs_path}")
    parent = os.path.dirname(abs_path)
    if parent:
        os.makedirs(parent, exist_ok=True)
    flags = os.O_WRONLY | os.O_CREAT | os.O_TRUNC | getattr(os, "O_NOFOLLOW", 0)
    fd = os.open(abs_path, flags, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as f:
        os.fchmod(f.fileno(), 0o600)
        json.dump(payload, f, indent=2)
        f.write("\n")
    return abs_path


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Write Pi models.json from CI LLM env vars.")
    parser.add_argument("--models-out", required=True, help="Output path for models.json")
    parser.add_argument(
        "--github-output",
        default=os.environ.get("GITHUB_OUTPUT", ""),
        help="Optional GITHUB_OUTPUT file path",
    )
    parser.add_argument(
        "--token-env",
        default="LOOPWRIGHT_PROVIDER_TOKEN",
        help="Environment variable holding the provider API token",
    )
    args = parser.parse_args(argv)

    if not ENV_NAME_RE.fullmatch(args.token_env):
        print(
            f"ERROR: --token-env must match ^[A-Z][A-Z0-9_]*$, got {args.token_env!r}",
            file=sys.stderr,
        )
        return 1

    raw_url = first_env("CI_GPUSTACK_LOOPWRIGHT_LLM_URL", "CI_GPUSTACK_CODEREVIEW_LLM_URL")
    raw_model = first_env("CI_GPUSTACK_LOOPWRIGHT_LLM_MODEL", "CI_GPUSTACK_CODEREVIEW_LLM_MODEL")

    if not raw_url and not raw_model:
        try:
            write_github_outputs(
                args.github_output,
                [
                    ("configured", "false"),
                    ("provider", ""),
                    ("model", ""),
                    ("thinking", ""),
                    ("api", ""),
                    ("base-url", ""),
                    ("models-json", ""),
                    ("provider-token-env", ""),
                ],
            )
        except (ValueError, OSError) as exc:
            print(f"ERROR: {exc}", file=sys.stderr)
            return 1
        print(json.dumps({"configured": False}))
        return 0

    if not raw_url or not raw_model:
        print(
            "ERROR: both LLM URL and LLM MODEL must be set when configuring gateway",
            file=sys.stderr,
        )
        return 1

    if any(ch.isspace() or ord(ch) < 32 for ch in raw_model) or len(raw_model) > 128:
        print(f"ERROR: invalid model identifier: {raw_model!r}", file=sys.stderr)
        return 1

    try:
        base_url = normalize_and_validate_url(raw_url)
        provider = infer_provider(
            base_url, first_env("CI_GPUSTACK_LOOPWRIGHT_LLM_PROVIDER")
        )
        use_anthropic = parse_bool_env(
            first_env(
                "CI_GPUSTACK_LOOPWRIGHT_LLM_USE_ANTHROPIC",
                "CI_GPUSTACK_CODEREVIEW_LLM_USE_ANTHROPIC",
            )
        )
        api = resolve_wire_api(
            first_env("CI_GPUSTACK_LOOPWRIGHT_LLM_PROTOCOL"),
            use_anthropic,
            provider,
        )
        thinking = resolve_thinking(
            first_env(
                "CI_GPUSTACK_LOOPWRIGHT_LLM_THINKING",
                "CI_GPUSTACK_CODEREVIEW_LLM_THINKING",
            ),
            first_env(
                "CI_GPUSTACK_LOOPWRIGHT_LLM_REASONING_EFFORT",
                "CI_GPUSTACK_CODEREVIEW_LLM_REASONING_EFFORT",
            ),
        )
        temperature = parse_float_param(
            first_env(
                "CI_GPUSTACK_LOOPWRIGHT_LLM_TEMPERATURE",
                "CI_GPUSTACK_CODEREVIEW_LLM_TEMPERATURE",
            ),
            "temperature",
            0.0,
            2.0,
        )
        top_p = parse_float_param(
            first_env(
                "CI_GPUSTACK_LOOPWRIGHT_LLM_TOP_P",
                "CI_GPUSTACK_CODEREVIEW_LLM_TOP_P",
            ),
            "top_p",
            0.0,
            1.0,
        )
        max_tokens = parse_int_param(
            first_env("CI_GPUSTACK_LOOPWRIGHT_LLM_MAX_TOKENS"),
            "max_tokens",
            1,
            1048576,
        )
        context_window = parse_int_param(
            first_env("CI_GPUSTACK_LOOPWRIGHT_LLM_CONTEXT_WINDOW_SIZE"),
            "context_window_size",
            1024,
            10000000,
        )
    except ValueError as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1

    reasoning = thinking != "off"
    model_entry: dict[str, object] = {
        "id": raw_model,
        "api": api,
        "baseUrl": base_url,
        "reasoning": reasoning,
    }
    if thinking == "max":
        model_entry["thinkingLevelMap"] = {"max": "high"}
    if context_window is not None:
        model_entry["contextWindow"] = context_window
    if max_tokens is not None:
        model_entry["maxTokens"] = max_tokens

    sampling: dict[str, float] = {}
    if temperature is not None:
        sampling["temperature"] = temperature
    if top_p is not None:
        sampling["top_p"] = top_p
    if sampling:
        model_entry["samplingParams"] = sampling

    if api == "openai-completions" and (
        provider in ("zai", "zai-cn", "zai-coding-cn")
        or raw_model.lower().startswith("glm-")
    ):
        model_entry["compat"] = {
            "thinkingFormat": "zai",
            "supportsReasoningEffort": True,
        }

    models_doc = {
        "providers": {
            provider: {
                "baseUrl": base_url,
                "api": api,
                "apiKey": f"${args.token_env}",
                "models": [model_entry],
            }
        }
    }

    try:
        written_path = write_models_file(args.models_out, models_doc)

        write_github_outputs(
            args.github_output,
            [
                ("configured", "true"),
                ("provider", provider),
                ("model", raw_model),
                ("thinking", thinking),
                ("api", api),
                ("base-url", base_url),
                ("models-json", written_path),
                ("provider-token-env", args.token_env),
                ("allow-custom-base-url", "true"),
            ],
        )
    except (ValueError, OSError) as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1

    summary = {
        "configured": True,
        "provider": provider,
        "model": raw_model,
        "thinking": thinking,
        "api": api,
        "baseUrl": base_url,
        "modelsJson": written_path,
        "providerTokenEnv": args.token_env,
        "allowCustomBaseUrl": True,
    }
    print(json.dumps(summary))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
