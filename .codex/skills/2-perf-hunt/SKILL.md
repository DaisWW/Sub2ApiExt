---
name: 2-perf-hunt
description: Use only when the user explicitly requests a full-project autonomous performance hunt for Sub2ApiExt. Do not trigger for a single symptom or ordinary review.
---

# Perf Hunt

仅在用户明确要求全项目性能扫描时使用；普通性能症状走常规工作流。确认目标服务、环境、
预算和是否只读；结论必须有查询量、并发、延迟、分配、goroutine 或容器指标等证据。

## Workflow

1. 映射同步/探测周期、SQL 窗口、HTTP、持久化、日志和容器启动路径。
2. 分批检查并记录范围；优先无界查询/响应、过量探测、并发抖动、重复扫描和噪声日志。
3. 报告影响、置信度、证据和验证方法；未知项标为待测量。
4. 不以削弱限流、代理/SSRF、租约、重试或一致性换性能；仅获授权后改动并复测。
