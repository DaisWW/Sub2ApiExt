"""独立授权框架入口；默认只运行本机虚构演示。"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

from browser import ChromeLogin
from flow import Credentials, LoginError, LoginFlow, Provider, ProxyHttpClient


ROOT = Path(__file__).resolve().parent


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description="独立 OAuth 登录框架；默认本机虚构演示。")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--demo", action="store_true", help="仅访问本机模拟授权站点和模拟代理（默认）")
    mode.add_argument("--live", action="store_true", help="使用已确认的 OAuth 配置和本地账户输入")
    parser.add_argument("--headless", action="store_true", help="仅允许本机 demo 无界面测试")
    parser.add_argument("--config", type=Path, default=ROOT / "config.json")
    parser.add_argument("--input", type=Path, default=ROOT / "input" / "account.json")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args(argv)
    if args.live and args.headless:
        parser.error("--headless 只能用于本机 demo")
    try:
        if args.live:
            provider = Provider.load(args.config)
            credentials = Credentials.load(args.input)
            if credentials.email.casefold().endswith(".invalid"):
                raise LoginError("示例邮箱只用于本地演示，不能提交到真实授权服务。")
            output = args.output or ROOT / "output" / "account.json"
            LoginFlow(provider, ProxyHttpClient(), ChromeLogin()).run(credentials, output)
        else:
            from demo import LocalFixture

            output = args.output or ROOT / "cache" / "demo-account.json"
            print("本机虚构演示：模拟代理只转发到本机，不向公网提交账户信息。")
            with LocalFixture() as fixture:
                browser = ChromeLogin(fixture=True, headless=args.headless)
                LoginFlow(fixture.provider, fixture.transport, browser).run(fixture.credentials, output)
                fixture.assert_complete()
            print("本机模拟步骤已完成；这不代表真实账户已通过授权。")
        print(f"JSON 输出位置：{output}")
        return 0
    except LoginError as exc:
        print(f"停止：{exc}", file=sys.stderr)
        return 1
    except ModuleNotFoundError:
        print("缺少运行依赖，请按 README 使用 requirements.txt 经本机 7897 代理安装。", file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        print("已取消；授权会话已关闭。", file=sys.stderr)
        return 130
    except Exception:
        print("执行失败；为避免泄露凭据，未输出异常正文。", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
