---
name: 2-perf-hunt
description: Use only when the user explicitly requests a full-project autonomous performance hunt for Sub2ApiExt. Do not trigger for a single symptom or ordinary review.
---

# Perf Hunt

仅在用户明确要求全项目性能扫描时使用；普通性能症状走常规工作流。默认先只读；
复用当前请求和此前会话中已确定的目标服务、环境、预算和授权。缺少预算不阻塞不访问生产、
不产生外部费用的只读静态审查。只有缺失信息会改变任务目标，或下一步需要新增授权时才提问。
已证实的性能结论必须有测量或可靠复现证据；静态候选问题给出代码路径、触发条件和验证方法，
标明待测量。

## Workflow

1. 映射同步/探测周期、SQL 窗口、HTTP、持久化、日志和容器启动路径。
2. 分批检查并记录范围；优先无界查询/响应、过量探测、并发抖动、重复扫描和噪声日志。
3. 报告影响、置信度、证据和验证方法；未知项标为待测量。
4. 不以削弱限流、代理/SSRF、租约、重试或一致性换性能；已有明确改动授权时完成范围内的修改并复测，
   无需再次询问。明确规定的操作时批准仍须遵守。
