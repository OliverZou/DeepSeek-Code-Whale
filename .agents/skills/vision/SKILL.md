---
name: vision
description: 调用通义千问 VL 多模态模型识别图片内容，支持本地图片和网络图片
---

# Vision — 独立识图 Skill

调用通义千问 VL 多模态模型识别图片内容。

## 前提

- Node.js 18+
- `DASHSCOPE_API_KEY` 环境变量（从 https://bailian.console.aliyun.com/ 获取）
- 可选：`VISION_MODEL` 环境变量（默认 `qwen3.5-omni-plus`）

## 用法

```bash
# 识别本地图片
node .agents/skills/vision/vision.js 图片路径 [问题]

# 识别网络图片
node .agents/skills/vision/vision.js --url 图片链接 [问题]
```

### 示例

```bash
# 默认：描述图片
node .agents/skills/vision/vision.js screenshot.png

# 带问题：具体分析
node .agents/skills/vision/vision.js chart.png "这张图展示了什么数据趋势？"

# 网络图片
node .agents/skills/vision/vision.js --url https://example.com/photo.jpg "图片里有什么？"
```

## 与 Team Engine 配合

在 Team Engine 的 worker 中，如果 agent 需要识图（例如分析图表、截图、UI 设计稿），
可以通过 spawn_subagent 调用此脚本，或在 worker prompt 中引用此能力。

## 配置

| 环境变量 | 说明 | 默认值 |
|----------|------|--------|
| `DASHSCOPE_API_KEY` | 阿里云 DashScope API Key | 内嵌默认 Key |
| `DASHSCOPE_BASE_URL` | API 端点地址 | `https://dashscope.aliyuncs.com/compatible-mode/v1` |
| `VISION_MODEL` | 视觉模型名 | `qwen3.5-omni-plus` |
