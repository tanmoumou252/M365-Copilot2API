#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
M365-Copilot2API 批量端口测试脚本
================================

人工构造 HTTP request 依次请求网关的各端口, 验证端点可用, 并检查响应中是否
返回「使用量」(usage) 响应值 (total_tokens / input_tokens 等)。流式与非流式
分别测试, 最后输出汇总结果表。

只要管理员密码即可完整测试, 不需要配置 API key 和模型:
  1. 用管理员密码登录 (POST /api/admin/login) 拿 session cookie
  2. 通过 /api/admin/keys 自动创建一个临时测试 API key
  3. 通过 /api/admin/models 自动探测一个可用的测试模型
  4. 用临时 key 测试 /v1/* 端点, 用管理员会话测试 /api/* 端点
  5. 测试结束后自动删除临时 key

配置来源 (优先级: 命令行 > .env > .env.example):
  M365_LISTEN        监听地址 (如 127.0.0.1:4141), 决定测试的基础 URL
  M365_ADMIN_PASSWORD 管理员密码 (必需)

可选覆盖 (不设置则自动创建/自动探测):
  --key              指定要测试的 API key, 不指定则自动创建临时 key
  --model            指定要测试的模型, 逗号分隔多个; 不指定则自动探测

命令行选项:
  --base, --key, --model, --admin-password, --timeout, --help

退出码: 0 = 全部通过, 1 = 有失败, 2 = 参数错误
依赖: 仅 Python 3 标准库 (urllib / json), 零第三方依赖。
"""

import argparse
import json
import os
import socket
import sys
import time
import urllib.error
import urllib.request

# Windows 控制台使用 UTF-8 输出, 避免 GBK/UTF-8 编码乱码。
for _stream in (sys.stdout, sys.stderr):
    try:
        _stream.reconfigure(encoding="utf-8", errors="replace")
    except (AttributeError, ValueError):
        pass

BASE_DEFAULT = "http://127.0.0.1:4141"
MODEL_DEFAULT = "gpt-5.6-sol"
PROMPT = "Reply with exactly: ok"
MAX_TOKENS = 50
TIMEOUT_DEFAULT = 90


# ---------------------------------------------------------------- env 解析

def load_env(path):
    """解析 KEY=VALUE 格式的 env 文件, 支持 # 注释与 $HOME 展开。"""
    env = {}
    try:
        with open(path, "r", encoding="utf-8") as f:
            for line in f:
                s = line.strip()
                if not s or s.startswith("#") or "=" not in s:
                    continue
                k, _, v = s.partition("=")
                k = k.strip()
                v = v.strip()
                if not k:
                    continue
                if len(v) >= 2 and v[0] == v[-1] and v[0] in ('"', "'"):
                    v = v[1:-1]
                home = os.path.expanduser("~")
                v = v.replace("${HOME}", home).replace("$HOME", home)
                env[k] = v
    except OSError:
        pass
    return env


def build_base(listen):
    """M365_LISTEN 的值 (如 127.0.0.1:4141) 拼成 http:// 基础 URL。"""
    listen = (listen or "").strip()
    if not listen:
        return BASE_DEFAULT
    if "://" in listen:
        return listen.rstrip("/")
    return "http://" + listen


def mask_key(key):
    if len(key) <= 12:
        return "*" * len(key)
    return key[:6] + "..." + key[-4:]


# ---------------------------------------------------------------- HTTP 封装

def http_call(method, url, headers=None, payload=None, timeout=TIMEOUT_DEFAULT):
    """发非流式请求, 返回 (status_code, body_text, elapsed_ms, error)。"""
    data = None
    if payload is not None:
        data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    start = time.time()
    try:
        resp = urllib.request.urlopen(req, timeout=timeout)
        code = getattr(resp, "code", None) or getattr(resp, "status", None)
        body = resp.read().decode("utf-8", "replace")
        return code, body, (time.time() - start) * 1000, None
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8", "replace")
        return e.code, body, (time.time() - start) * 1000, None
    except (urllib.error.URLError, socket.timeout, OSError) as e:
        return None, "", (time.time() - start) * 1000, "{}".format(e)


def http_open_stream(method, url, headers=None, payload=None, timeout=TIMEOUT_DEFAULT):
    """发起流式请求, 返回 (response_obj, status_code, elapsed_ms, error)。"""
    data = None
    if payload is not None:
        data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    start = time.time()
    try:
        resp = urllib.request.urlopen(req, timeout=timeout)
        code = getattr(resp, "code", None) or getattr(resp, "status", None)
        return resp, code, (time.time() - start) * 1000, None
    except urllib.error.HTTPError as e:
        return None, e.code, (time.time() - start) * 1000, None
    except (urllib.error.URLError, socket.timeout, OSError) as e:
        return None, None, (time.time() - start) * 1000, "{}".format(e)


def read_sse(resp, timeout=TIMEOUT_DEFAULT):
    """逐行读 SSE 流, 返回 (usage_chunk_count, reached_done)。"""
    usage_chunks = 0
    done = False
    deadline = time.time() + timeout
    try:
        while time.time() < deadline:
            line = resp.readline()
            if not line:
                break
            text = line.decode("utf-8", "replace").strip()
            if not text or text.startswith(":"):
                continue
            if text.startswith("data:"):
                data = text[len("data:"):].strip()
                if data == "[DONE]":
                    done = True
                    break
                try:
                    obj = json.loads(data)
                    if isinstance(obj, dict) and obj.get("usage"):
                        usage_chunks += 1
                except ValueError:
                    pass
    except (socket.timeout, OSError):
        pass
    finally:
        try:
            resp.close()
        except Exception:
            pass
    return usage_chunks, done


def parse_json(text):
    try:
        return json.loads(text)
    except (ValueError, TypeError):
        return None


def status_label(status):
    if status is None:
        return "ERR"
    return "{} OK".format(status) if 200 <= status < 400 else "{} ERR".format(status)


# ---------------------------------------------------------------- 测试用例

def make_result(seq, name, method, stream, status, time_ms, usage, result, note=""):
    return {"seq": seq, "name": name, "method": method, "stream": stream,
            "status": status, "time_ms": time_ms, "usage": usage,
            "result": result, "note": note}


def test_health(ctx, seq):
    code, body, ms, err = http_call("GET", ctx["base"] + "/api/health",
                                    headers=ctx["admin_headers"], timeout=ctx["timeout"])
    if err:
        return make_result(seq, "/api/health", "GET", False, None, ms, "-", "FAIL", err)
    data = parse_json(body)
    ok = code == 200 and isinstance(data, dict) and data.get("status") == "ok"
    note = "" if ok else "status=%r" % data if isinstance(data, dict) else body[:200]
    return make_result(seq, "/api/health", "GET", False, code, ms, "N/A",
                       "PASS" if ok else "FAIL", note)


def test_models(ctx, seq, model):
    code, body, ms, err = http_call("GET", ctx["base"] + "/v1/models",
                                    headers=ctx["api_headers"], timeout=ctx["timeout"])
    name = "/v1/models"
    if err:
        return make_result(seq, name, "GET", False, None, ms, "-", "FAIL", err)
    data = parse_json(body)
    ids = []
    if isinstance(data, dict):
        for m in data.get("data") or []:
            if isinstance(m, dict) and m.get("id"):
                ids.append(m["id"])
    ok = code in (200, 201) and model in ids
    note = "" if ok else "model %r not in catalog" % model if code in (200, 201) else body[:200]
    return make_result(seq, name, "GET", False, code, ms, "N/A",
                       "PASS" if ok else "FAIL", note)


def test_chat(ctx, seq, model, stream):
    url = ctx["base"] + "/v1/chat/completions" + ("?stream=true" if stream else "")
    payload = {"model": model,
               "messages": [{"role": "user", "content": PROMPT}],
               "max_tokens": MAX_TOKENS, "stream": stream}
    if stream:
        payload["stream_options"] = {"include_usage": True}
    name = "/v1/chat/completions (%s)" % model
    if stream:
        resp, code, ms, err = http_open_stream("POST", url, headers=ctx["api_headers"],
                                               payload=payload, timeout=ctx["timeout"])
        if err or resp is None:
            return make_result(seq, name, "POST", True, code, ms, "-", "FAIL",
                               err or "no response stream")
        usage_chunks, done = read_sse(resp, ctx["timeout"])
        ok = done and code in (200, 201)
        note = "" if ok else ("SSE did not end with [DONE]" if code in (200, 201) else "status=%s" % code)
        return make_result(seq, name, "POST", True, code, ms,
                           "chunks:%d" % usage_chunks, "PASS" if ok else "FAIL", note)
    code, body, ms, err = http_call("POST", url, headers=ctx["api_headers"],
                                    payload=payload, timeout=ctx["timeout"])
    if err:
        return make_result(seq, name, "POST", False, None, ms, "-", "FAIL", err)
    data = parse_json(body)
    usage = data.get("usage") if isinstance(data, dict) else None
    total = usage.get("total_tokens") if isinstance(usage, dict) else None
    ok = code in (200, 201) and isinstance(usage, dict) and total is not None
    note = "" if ok else body[:200]
    return make_result(seq, name, "POST", False, code, ms,
                       "total:%s" % total if total is not None else "-",
                       "PASS" if ok else "FAIL", note)


def test_responses(ctx, seq, model):
    url = ctx["base"] + "/v1/responses"
    payload = {"model": model, "input": PROMPT}
    name = "/v1/responses (%s)" % model
    code, body, ms, err = http_call("POST", url, headers=ctx["api_headers"],
                                    payload=payload, timeout=ctx["timeout"])
    if err:
        return make_result(seq, name, "POST", False, None, ms, "-", "FAIL", err)
    data = parse_json(body)
    usage = data.get("usage") if isinstance(data, dict) else None
    total = usage.get("total_tokens") if isinstance(usage, dict) else None
    ok = code in (200, 201) and isinstance(usage, dict) and len(usage) > 0
    note = "" if ok else body[:200]
    return make_result(seq, name, "POST", False, code, ms,
                       "total:%s" % total if total is not None else "-",
                       "PASS" if ok else "FAIL", note)


def test_messages(ctx, seq, model):
    url = ctx["base"] + "/v1/messages"
    payload = {"model": model,
               "messages": [{"role": "user", "content": PROMPT}],
               "max_tokens": MAX_TOKENS}
    name = "/v1/messages (%s)" % model
    code, body, ms, err = http_call("POST", url, headers=ctx["api_headers"],
                                    payload=payload, timeout=ctx["timeout"])
    if err:
        return make_result(seq, name, "POST", False, None, ms, "-", "FAIL", err)
    data = parse_json(body)
    usage = data.get("usage") if isinstance(data, dict) else None
    ok = code in (200, 201) and isinstance(usage, dict) and "input_tokens" in usage
    note = "" if ok else body[:200]
    inp = usage.get("input_tokens") if isinstance(usage, dict) else None
    return make_result(seq, name, "POST", False, code, ms,
                       "in:%s" % inp if inp is not None else "-",
                       "PASS" if ok else "FAIL", note)


def test_api_chat(ctx, seq):
    url = ctx["base"] + "/api/chat"
    payload = {"message": PROMPT}
    code, body, ms, err = http_call("POST", url, headers=ctx["admin_headers"],
                                    payload=payload, timeout=ctx["timeout"])
    if err:
        return make_result(seq, "/api/chat", "POST", False, None, ms, "-", "FAIL", err)
    data = parse_json(body)
    ok = code == 200 and isinstance(data, dict) and data.get("status") == "ok" and "text" in data
    note = "" if ok else body[:200]
    return make_result(seq, "/api/chat", "POST", False, code, ms, "N/A",
                       "PASS" if ok else "FAIL", note)


def test_api_usage(ctx, seq):
    url = ctx["base"] + "/api/usage?days=7"
    code, body, ms, err = http_call("GET", url, headers=ctx["admin_headers"],
                                    timeout=ctx["timeout"])
    if err:
        return make_result(seq, "/api/usage", "GET", False, None, ms, "-", "FAIL", err)
    data = parse_json(body)
    stats = data.get("stats") if isinstance(data, dict) else None
    ok = code == 200 and isinstance(stats, dict) and len(stats) > 0
    usage_str = "-"
    if ok and isinstance(stats.get("summary"), dict):
        sm = stats["summary"]
        usage_str = "req:%s tok:%s" % (sm.get("requests"), sm.get("tokens"))
    note = "" if ok else body[:200]
    return make_result(seq, "/api/usage", "GET", False, code, ms, usage_str,
                       "PASS" if ok else "FAIL", note)


# ---------------------------------------------------------------- 汇总输出

def print_row(r):
    stream_mark = "SSE " if r["stream"] else "--- "
    status = status_label(r["status"])
    print("  [%02d] %-6s %-42s %s %-8s %8dms  usage=%-12s %s"
          % (r["seq"], r["method"], r["name"][:42], stream_mark, status,
             int(r["time_ms"]), r["usage"], r["result"]))
    if r["note"]:
        print("        note: %s" % r["note"][:160])


def print_summary(models, base, key, admin_key_id, admin_ok, results):
    print()
    print("=" * 96)
    print("测试模型 : %s" % ", ".join(models))
    print("API key  : %s" % mask_key(key))
    if admin_key_id:
        print("临时 key : id=%s (测试结束已删除)" % admin_key_id)
    print("基础地址 : %s" % base)
    print("管理员   : %s" % ("已登录" if admin_ok else "跳过 (/api/* 端点将 SKIP)"))
    print("-" * 96)
    print("  #  端点                          方法  流式  状态码   耗时      usage         结果")
    print("-" * 96)
    for r in results:
        print("  %-3d %-31s %-6s %-4s %-8s %7dms  %-14s %s"
              % (r["seq"], r["name"][:31], r["method"], "是" if r["stream"] else "否",
                 status_label(r["status"]), int(r["time_ms"]), r["usage"], r["result"]))
    print("-" * 96)
    passed = sum(1 for r in results if r["result"] == "PASS")
    failed = sum(1 for r in results if r["result"] == "FAIL")
    skipped = sum(1 for r in results if r["result"] == "SKIP")
    print("  通过: %d    失败: %d    跳过: %d    共: %d" % (passed, failed, skipped, len(results)))
    print("=" * 96)


# ---------------------------------------------------------------- 主流程

def parse_args(argv):
    p = argparse.ArgumentParser(
        prog="test_api.py",
        description="M365-Copilot2API 批量端口 / 使用量 (usage) 测试脚本。\n"
                    "只需要管理员密码即可自动创建临时 API key 并探测模型完成全部测试。",
        epilog="变量来源优先级: 命令行 > .env > .env.example\n"
               "必需变量: M365_ADMIN_PASSWORD\n"
               "可选覆盖: --key / --model (不指定则自动创建 / 自动探测)")
    p.add_argument("--base", help="基础 URL, 如 http://127.0.0.1:4141 (默认读 M365_LISTEN)")
    p.add_argument("--key", help="指定要测试的 API key; 不指定则自动创建临时 key")
    p.add_argument("--model", help="指定要测试的模型名, 逗号分隔多个; 不指定则自动探测")
    p.add_argument("--admin-password", help="管理员密码 (默认读 M365_ADMIN_PASSWORD)")
    p.add_argument("--timeout", type=int, default=TIMEOUT_DEFAULT, help="单请求超时秒数 (默认 %d)" % TIMEOUT_DEFAULT)
    return p.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)

    env = {}
    base_dir = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    env.update(load_env(os.path.join(base_dir, ".env.example")))
    env.update(load_env(os.path.join(base_dir, ".env")))

    base = args.base or build_base(env.get("M365_LISTEN", ""))
    models_raw = args.model or ""  # 可选: 指定模型
    admin_pwd = args.admin_password or env.get("M365_ADMIN_PASSWORD", "")
    timeout = args.timeout

    if not admin_pwd:
        print("错误: 未提供管理员密码。")
        print("  请设置 .env 中的 M365_ADMIN_PASSWORD, 或使用 --admin-password 参数。")
        return 2

    print("M365-Copilot2API 批量端口测试")
    print("  基础地址 : %s" % base)
    print()

    # 1. 管理员登录
    cookie = _admin_login(base, admin_pwd, timeout)
    if not cookie:
        print("错误: 管理员登录失败。")
        print("  请检查 .env 中的 M365_ADMIN_PASSWORD 是否正确。")
        return 1
    ctx = {
        "base": base,
        "admin_headers": {"Cookie": "m365_admin_session=" + cookie,
                          "Content-Type": "application/json"},
        "api_headers": None,
        "timeout": timeout,
    }
    admin_ok = True
    print("  管理员登录成功")

    # 2. 解析 API key: 指定则用指定的, 否则自动创建临时 key
    admin_key_id = None
    user_key = args.key or env.get("M365_TEST_API_KEY", "")
    if user_key:
        key = user_key
        print("  使用指定的 API key: %s" % mask_key(key))
    else:
        key, admin_key_id = _admin_create_key(ctx["admin_headers"], base, timeout)
        if not key:
            print("错误: 自动创建临时 API key 失败, 无法测试 /v1/* 端点。")
            print("  可用 --key 手动指定一个有效 key。")
            return 1
        print("  已自动创建临时 API key: %s (id=%s)" % (mask_key(key), admin_key_id))
    ctx["api_headers"] = {"Authorization": "Bearer " + key,
                          "Content-Type": "application/json"}
    print()

    # 3. 解析模型: 指定则用指定的, 否则自动探测
    if models_raw:
        models = [m.strip() for m in str(models_raw).split(",") if m.strip()]
    else:
        models = _admin_detect_models(ctx["admin_headers"], base, timeout)
        if not models:
            print("错误: 自动探测模型失败, 请用 --model 指定测试模型。")
            if admin_key_id:
                _admin_delete_key(ctx["admin_headers"], base, admin_key_id, timeout)
            return 1
        print("  已自动探测测试模型: %s" % ", ".join(models))
    if not models:
        print("错误: 未提供测试模型, 请用 --model 指定 (逗号分隔可测多个)。")
        if admin_key_id:
            _admin_delete_key(ctx["admin_headers"], base, admin_key_id, timeout)
        return 2
    print()

    # 4. 组装测试用例
    cases = []
    cases.append(("health", None, False))
    for m in models:
        cases.append(("models", m, False))
        cases.append(("chat", m, False))
        cases.append(("chat", m, True))
        cases.append(("responses", m, False))
        cases.append(("messages", m, False))
    cases.append(("api_chat", None, False))
    cases.append(("api_usage", None, False))

    results = []
    seq = 0
    for kind, model, stream in cases:
        seq += 1
        if kind in ("health", "api_chat", "api_usage"):
            if not admin_ok:
                name = {"health": "/api/health", "api_chat": "/api/chat", "api_usage": "/api/usage"}[kind]
                results.append(make_result(seq, name, "GET" if kind != "api_chat" else "POST",
                                           False, None, 0, "-", "SKIP", "需要管理员登录"))
                continue
        try:
            if kind == "health":
                r = test_health(ctx, seq)
            elif kind == "models":
                r = test_models(ctx, seq, model)
            elif kind == "chat":
                r = test_chat(ctx, seq, model, stream)
            elif kind == "responses":
                r = test_responses(ctx, seq, model)
            elif kind == "messages":
                r = test_messages(ctx, seq, model)
            elif kind == "api_chat":
                r = test_api_chat(ctx, seq)
            elif kind == "api_usage":
                r = test_api_usage(ctx, seq)
            else:
                continue
        except Exception as e:  # 单个用例异常不中断整批
            r = make_result(seq, "/%s (%s)" % (kind, model or ""), "?", False,
                            None, 0, "-", "FAIL", "unexpected: %r" % e)
        results.append(r)
        print_row(r)

    # 5. 删除自动创建的临时 key
    if admin_key_id:
        _admin_delete_key(ctx["admin_headers"], base, admin_key_id, timeout)
        print()
        print("  已删除临时 API key: %s" % admin_key_id)

    print_summary(models, base, key, admin_key_id, admin_ok, results)

    failed = any(r["result"] == "FAIL" for r in results)
    return 1 if failed else 0


def _admin_login(base, password, timeout):
    """POST /api/admin/login, 成功返回 session cookie, 失败返回 None。"""
    req = urllib.request.Request(
        base + "/api/admin/login",
        data=json.dumps({"password": password}).encode("utf-8"),
        headers={"Content-Type": "application/json"}, method="POST")
    try:
        resp = urllib.request.urlopen(req, timeout=timeout)
    except urllib.error.HTTPError as e:
        resp = e
    except Exception:
        return None
    set_cookie = resp.headers.get("Set-Cookie")
    if not set_cookie:
        return None
    for part in set_cookie.split(";"):
        part = part.strip()
        if part.startswith("m365_admin_session="):
            return part[len("m365_admin_session="):]
    return None


def _admin_create_key(admin_headers, base, timeout):
    """POST /api/admin/keys 创建临时 key, 返回 (raw_key, id)。"""
    code, body, ms, err = http_call("POST", base + "/api/admin/keys",
                                    headers=admin_headers,
                                    payload={"name": "batch-test-temp"}, timeout=timeout)
    if err or code != 200:
        return None, None
    data = parse_json(body)
    if not (isinstance(data, dict) and data.get("key") and data.get("record")):
        return None, None
    return data["key"], data["record"].get("id")


def _admin_delete_key(admin_headers, base, key_id, timeout):
    """DELETE /api/admin/keys?id=... 删除临时 key。"""
    http_call("DELETE", base + "/api/admin/keys?id=" + str(key_id),
              headers=admin_headers, timeout=timeout)


def _admin_detect_models(admin_headers, base, timeout):
    """GET /api/admin/models, 返回单个测试模型 (优先默认模型, 否则第一个)。"""
    code, body, ms, err = http_call("GET", base + "/api/admin/models",
                                    headers=admin_headers, timeout=timeout)
    if err or code != 200:
        return []
    data = parse_json(body)
    if not isinstance(data, dict):
        return []
    models = []
    for m in data.get("data") or []:
        if isinstance(m, dict) and m.get("id"):
            models.append(m["id"])
    if not models:
        return []
    # 只挑一个模型测试, 避免对全部模型逐一请求导致耗时过长
    return [MODEL_DEFAULT] if MODEL_DEFAULT in models else [models[0]]


if __name__ == "__main__":
    sys.exit(main())
