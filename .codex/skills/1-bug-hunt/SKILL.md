---
name: 1-bug-hunt
description: Use only when the user explicitly requests a full-project autonomous bug hunt for Sub2ApiExt. Do not trigger for an ordinary bug report or a scoped review.
---

# Bug Hunt

仅在用户明确要求全项目 Bug 扫描时使用；普通故障和局部审查走常规工作流。默认先只读；
复用当前请求和此前会话中已确定的范围、预算和授权。缺少预算不阻塞不访问生产、
不产生外部费用的只读静态审查。只有缺失信息会改变任务目标，或下一步需要新增授权时才提问。
真实凭据、生产 PostgreSQL、卷和上游调用不得作为测试夹具。

## Workflow

1. 映射两个服务的入口、状态、SQL、HTTP、goroutine、租约、重试和部署脚本。
2. 分批检查并记录已覆盖/跳过范围；优先数据丢失、重复写入、密钥泄露、SSRF、泄漏、水位和只读边界。
3. 按 P0/P1/P2/P3 报告触发条件、影响、文件/行号、证据和复现/推理。
4. 仅审查请求只报告结果；已有明确修复授权时完成范围内的修改和验证，无需再次询问。
   明确规定的操作时批准仍须遵守。
