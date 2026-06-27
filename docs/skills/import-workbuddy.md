# WorkBuddy 数据导入技能

从 WorkBuddy 的 COS 市场（`acc-1258344699.cos.accelerate.myqcloud.com/workbuddy/expert-marketplace`）下载 agent MD 文件和团队信息，转换为 Whale Pod 的 experts/agents/teams 格式。

## 前置条件

- Python 3 + PyYAML（`pip install pyyaml`）
- WorkBuddy 的 manifest 文件（本地缓存或在线获取）
- 网络可访问 COS 地址

## 数据源

### Manifest 地址

```
https://acc-1258344699.cos.accelerate.myqcloud.com/workbuddy/expert-marketplace/expert_center.json
```

### 本地缓存路径

```
~/.workbuddy/app/cache/experts/manifest.json
```

### Agent MD 下载 URL 模式

```
{BASE_URL}/plugins/{plugin-name}/agents/{agent-name}.md
```

其中 `BASE_URL = https://acc-1258344699.cos.accelerate.myqcloud.com/workbuddy/expert-marketplace`

## Manifest 结构

```json
{
  "categories": [
    { "id": "01-ProductDesign", "name": { "en": "Product Design", "zh": "产品设计" } }
  ],
  "experts": [
    {
      "id": "ContentCreator",
      "categoryId": "06-ContentCreative",
      "expertType": "agent" | "team",
      "agentName": "content-creator",
      "displayName": { "en": "Kai", "zh": "文博凯" },
      "profession": { "en": "Content Creator", "zh": "内容创作专家" },
      "description": { "en": "...", "zh": "..." },
      "promptFile": "/plugins/content-creator/agents/content-creator.md",
      "tags": [ { "zh": "内容策略", "en": "Content Strategy" } ],
      "members": [  // team 类型才有
        { "id": "software-team-lead", "role": "lead", "profession": { "zh": "交付总监" }, "promptFile": "..." }
      ]
    }
  ]
}
```

## Whale Pod 目录结构

```
bin/
├── agents/
│   └── workbuddy/              ← 单专家 agent MD，按领域分目录
│       ├── 产品设计/
│       │   └── ui-designer.md
│       ├── 技术工程/
│       │   └── senior-developer.md
│       └── ...
├── experts/                    ← 专家定义 YAML，按领域分文件
│   ├── 产品设计.yaml
│   ├── 技术工程.yaml
│   └── ...
└── teams/                      ← 团队定义 + team-local agents
    └── 软件开发团队/
        ├── team.yaml
        └── agents/
            ├── software-team-lead.md
            └── ...
```

## 转换规则

### 1. Agent MD → `agents/workbuddy/{领域}/{agent-name}.md`

- 直接下载，原样保存
- 领域目录名 = manifest 中 `categoryId` 对应的 `name.zh`
- 404 的跳过（部分新上架或下架的插件无文件）

### 2. Expert YAML → `experts/{领域}.yaml`

从 manifest 的 `expertType: "agent"` 条目提取：

| Expert 字段 | 来源 |
|-------------|------|
| `name` | `profession.zh`（优先）或 `displayName.zh` |
| `name_en` | `agentName` |
| `agent` | `agentName`（解析器按文件名匹配） |
| `icon` | 空（后续可从 avatar 提取） |
| `description` | `description.zh` |
| `domains` | `[categoryId 对应的 name.zh]` |
| `skills` | `tags[].zh`（取前 3 个） |

文件级字段：

| 字段 | 来源 |
|------|------|
| `domain` | `categoryId` 对应的 `name.zh` |
| `domain_en` | `categoryId` |
| `icon` | 空 |

### 3. Team YAML → `teams/{团队名}/team.yaml`

从 manifest 的 `expertType: "team"` 条目提取：

| Team 字段 | 来源 |
|-----------|------|
| `label` | `displayName.zh` |
| `category` | `categoryId` 对应的 `name.zh` |
| `leader.role` | `members[role=lead].id` |
| `roles` | 所有 `members[].id` |
| `capabilities` | `description.zh` + `tags[].zh` |

### 4. Team-local agents → `teams/{团队名}/agents/{agent-name}.md`

- 从 manifest 的 `members[].promptFile` 下载
- 按插件名分目录下载后，移动到对应 team 目录

## WorkBuddy 分类（13 个）

| ID | 中文 |
|----|------|
| 01-ProductDesign | 产品设计 |
| 02-Engineering | 技术工程 |
| 03-GameSpatial | 游戏空间 |
| 04-DataAI | 数据智能 |
| 05-MarketingGrowth | 营销增长 |
| 06-ContentCreative | 内容创作 |
| 07-SalesCommerce | 销售商务 |
| 08-FinanceInvestment | 金融投资 |
| 09-OperationsHR | 运营人力 |
| 10-ProjectQuality | 项目质量 |
| 11-SecurityCompliance | 法务安全 |
| 12-IndustryConsultant | 行业顾问 |
| 13-TencentZone | 腾讯专区 |

## 导入脚本

### 步骤 1：下载单专家 agent MD + 生成 experts YAML

```python
# import_workbuddy_experts.py
import json, os, urllib.request, ssl, time, yaml

MANIFEST_PATH = r'C:\Users\oliver-PC\.workbuddy\app\cache\experts\manifest.json'
AGENTS_DIR = r'D:\src\DeepSeek-Code-Whale\bin\agents\workbuddy'
EXPERTS_DIR = r'D:\src\DeepSeek-Code-Whale\bin\experts'
BASE_URL = 'https://acc-1258344699.cos.accelerate.myqcloud.com/workbuddy/expert-marketplace'

ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE

with open(MANIFEST_PATH, 'r', encoding='utf-8') as f:
    data = json.load(f)

experts = [e for e in data.get('experts', []) if e.get('expertType') == 'agent']
cats = data.get('categories', [])
cat_map = {c['id']: c['name']['zh'] for c in cats}

by_category = {}
for e in experts:
    cid = e.get('categoryId', '')
    zh_cat = cat_map.get(cid, cid)
    if zh_cat not in by_category:
        by_category[zh_cat] = []
    by_category[zh_cat].append(e)

os.makedirs(AGENTS_DIR, exist_ok=True)
os.makedirs(EXPERTS_DIR, exist_ok=True)

downloaded = 0
failed = 0

for zh_cat, cat_experts in sorted(by_category.items()):
    for e in cat_experts:
        agent_name = e.get('agentName', '')
        pf = e.get('promptFile', '')
        if not agent_name or not pf:
            continue
        url = BASE_URL + pf
        agent_dir = os.path.join(AGENTS_DIR, zh_cat)
        os.makedirs(agent_dir, exist_ok=True)
        agent_file = os.path.join(agent_dir, agent_name + '.md')
        if not os.path.exists(agent_file):
            try:
                req = urllib.request.Request(url)
                with urllib.request.urlopen(req, context=ctx, timeout=15) as resp:
                    content = resp.read().decode('utf-8')
                with open(agent_file, 'w', encoding='utf-8') as f:
                    f.write(content)
                downloaded += 1
                time.sleep(0.1)
            except Exception as ex:
                failed += 1
                print(f'  FAILED: {agent_name} - {ex}')

print(f'Downloaded: {downloaded}, Failed: {failed}')

# Generate experts YAML
for zh_cat, cat_experts in sorted(by_category.items()):
    cat_id = ''
    for c in cats:
        if c['name']['zh'] == zh_cat:
            cat_id = c['id']
            break
    expert_entries = []
    for e in cat_experts:
        agent_name = e.get('agentName', '')
        profession_zh = e.get('profession', {}).get('zh', '')
        display_zh = e.get('displayName', {}).get('zh', '')
        desc_zh = e.get('description', {}).get('zh', '')
        tags = e.get('tags', [])
        tag_zh_list = [t.get('zh', '') for t in tags if t.get('zh')]
        name = profession_zh or display_zh or agent_name
        skills = tag_zh_list[:3]
        expert_entries.append({
            'name': name,
            'name_en': agent_name,
            'agent': agent_name,
            'icon': '',
            'description': desc_zh,
            'domains': [zh_cat],
            'skills': skills,
        })
    ef = {
        'domain': zh_cat,
        'domain_en': cat_id,
        'icon': '',
        'experts': expert_entries,
    }
    yaml_path = os.path.join(EXPERTS_DIR, zh_cat + '.yaml')
    with open(yaml_path, 'w', encoding='utf-8') as f:
        yaml.dump(ef, f, allow_unicode=True, default_flow_style=False, sort_keys=False)
    print(f'Generated: {zh_cat}.yaml ({len(expert_entries)} experts)')
```

### 步骤 2：下载团队 agent MD + 生成 team.yaml

```python
# import_workbuddy_teams.py
import json, os, urllib.request, ssl, time, yaml, shutil

MANIFEST_PATH = r'C:\Users\oliver-PC\.workbuddy\app\cache\experts\manifest.json'
AGENTS_DIR = r'D:\src\DeepSeek-Code-Whale\bin\agents\workbuddy\teams'
TEAMS_DIR = r'D:\src\DeepSeek-Code-Whale\bin\teams'
BASE_URL = 'https://acc-1258344699.cos.accelerate.myqcloud.com/workbuddy/expert-marketplace'

ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE

with open(MANIFEST_PATH, 'r', encoding='utf-8') as f:
    data = json.load(f)

teams = [e for e in data.get('experts', []) if e.get('expertType') == 'team']
cats = data.get('categories', [])
cat_map = {c['id']: c['name']['zh'] for c in cats}

downloaded = 0
failed = 0

def download_agent(prompt_file):
    global downloaded, failed
    if not prompt_file:
        return
    url = BASE_URL + prompt_file
    filename = prompt_file.split('/')[-1]
    subdir = prompt_file.split('/')[-3]
    agent_dir = os.path.join(AGENTS_DIR, subdir)
    os.makedirs(agent_dir, exist_ok=True)
    agent_file = os.path.join(agent_dir, filename)
    if os.path.exists(agent_file):
        return
    try:
        req = urllib.request.Request(url)
        with urllib.request.urlopen(req, context=ctx, timeout=15) as resp:
            content = resp.read().decode('utf-8')
        with open(agent_file, 'w', encoding='utf-8') as f:
            f.write(content)
        downloaded += 1
        time.sleep(0.05)
    except Exception as ex:
        failed += 1

for t in teams:
    download_agent(t.get('promptFile', ''))
    for m in t.get('members', []):
        download_agent(m.get('promptFile', ''))

print(f'Downloaded: {downloaded}, Failed: {failed}')

# Generate team.yaml and move agents to team dirs
os.makedirs(TEAMS_DIR, exist_ok=True)

for t in teams:
    team_zh = t.get('displayName', {}).get('zh', t.get('id', ''))
    desc_zh = t.get('description', {}).get('zh', '')
    lead_agent = t.get('agentName', '')
    members = t.get('members', [])
    tags = t.get('tags', [])
    tag_zh = [tg.get('zh', '') for tg in tags if tg.get('zh')]
    cat_id = t.get('categoryId', '')
    cat_zh = cat_map.get(cat_id, '')

    member_agents = []
    for m in members:
        m_agent = m.get('id', '')
        if m.get('role') == 'lead':
            lead_agent = m_agent
        else:
            member_agents.append(m_agent)

    all_roles = [lead_agent] + member_agents
    capabilities = []
    if desc_zh:
        capabilities.append(desc_zh)
    if tag_zh:
        capabilities.extend(tag_zh)

    team_config = {
        'label': team_zh,
        'category': cat_zh,
        'leader': {'role': lead_agent},
        'roles': all_roles,
    }
    if capabilities:
        team_config['capabilities'] = capabilities

    team_dir = os.path.join(TEAMS_DIR, team_zh)
    os.makedirs(team_dir, exist_ok=True)
    agents_dir = os.path.join(team_dir, 'agents')
    os.makedirs(agents_dir, exist_ok=True)

    # Move agent MDs from staging to team-local agents/
    plugin_name = t.get('plugin', '')
    staging = os.path.join(AGENTS_DIR, plugin_name)
    if os.path.isdir(staging):
        for f in os.listdir(staging):
            if f.endswith('.md'):
                shutil.copy2(os.path.join(staging, f), os.path.join(agents_dir, f))

    yaml_path = os.path.join(team_dir, 'team.yaml')
    with open(yaml_path, 'w', encoding='utf-8') as f:
        yaml.dump(team_config, f, allow_unicode=True, default_flow_style=False, sort_keys=False)
    print(f'Generated: {team_zh}/team.yaml (lead: {lead_agent}, members: {len(member_agents)})')
```

### 步骤 3：合并自有 agent 的专家条目

如果 Whale 自有 agent 需要加入 experts YAML，追加到对应领域文件中（避免与 WorkBuddy 同名 agent 重复）：

```python
# merge_our_experts.py
import os, yaml

experts_dir = r'D:\src\DeepSeek-Code-Whale\bin\experts'

our_experts = {
    '技术工程': [
        {'name': '后端工程师', 'name_en': 'backend-engineer', 'agent': 'backend-engineer',
         'icon': '⚙', 'description': '负责后端服务 API 开发和业务逻辑实现',
         'domains': ['技术工程'], 'skills': ['Go', 'Python', 'API']},
        # ... 更多自有 agent
    ],
    # ... 更多领域
}

for domain, entries in our_experts.items():
    path = os.path.join(experts_dir, domain + '.yaml')
    with open(path, 'r', encoding='utf-8') as f:
        data = yaml.safe_load(f)
    existing_agents = {e.get('agent', '') for e in data.get('experts', [])}
    for entry in entries:
        if entry['agent'] not in existing_agents:
            data['experts'].append(entry)
    with open(path, 'w', encoding='utf-8') as f:
        yaml.dump(data, f, allow_unicode=True, default_flow_style=False, sort_keys=False)
```

## 注意事项

- COS 下载不需要认证，但需要网络可达
- 部分 agent MD 会 404（新上架或下架），脚本自动跳过
- `agent` 字段只用文件名（不含路径），Whale 的 agent 解析器会递归搜索子目录
- team-local agents 放在 `teams/{团队名}/agents/` 下，Team Engine 优先从此加载
- experts YAML 的 `domains` 字段支持多值，实现跨领域专家

## 已知陷阱与经验

### 1. 团队 agents 目录错位 bug

**现象**：多个团队的 `agents/` 目录包含了属于其他团队的 agent MD 文件。

**根因**：步骤 2 的脚本用 `shutil.copy2`（而非 move）将 staging 目录的文件复制到 team 目录，但 staging 按 plugin 名组织而非 team 名。当多个 plugin 的 agent 文件被复制到同一个目标目录时，后复制的会与先复制的混在一起。

**典型案例**：
- MVP开发专家团的 agents/ 错误包含工程保障团队(6) + PPT大纲团队(7) 的 agent
- 内容创作专家团的 agents/ 错误包含袋鼠帝团队(6) 的 agent
- 投资大师专家团的 agents/ 错误包含腾讯自选股团队(7) 的 agent
- 交易分析团队多出投资大师专家团的 21 个 agent

**修复方法**：对比 team.yaml 的 roles 列表与 agents/ 目录中的实际文件，删除不属于该角色的文件：

```python
import os, yaml

teams_dir = r'D:\src\DeepSeek-Code-Whale\bin\teams'
for team_name in os.listdir(teams_dir):
    team_yaml = os.path.join(teams_dir, team_name, 'team.yaml')
    agents_dir = os.path.join(teams_dir, team_name, 'agents')
    if not os.path.exists(team_yaml) or not os.path.exists(agents_dir):
        continue
    with open(team_yaml, 'r', encoding='utf-8') as f:
        data = yaml.safe_load(f)
    roles = set(data.get('roles', []))
    existing = {f[:-3] for f in os.listdir(agents_dir) if f.endswith('.md')}
    misplaced = existing - roles
    for m in misplaced:
        os.remove(os.path.join(agents_dir, f'{m}.md'))
        print(f'DELETED: {team_name}/{m}.md')
```

**预防**：导入脚本应使用 `shutil.move` 而非 `shutil.copy2`，或在复制前清空目标目录。

### 2. COS 404 与本地 WorkBuddy 缓存

**现象**：manifest 中有条目且 promptFile 非空，但 COS 下载返回 404。

**根因**：WorkBuddy 的 manifest 注册了专家，但对应的 agent MD 文件可能：
- 尚未上传到 COS（新上架的专家）
- 已从 COS 下架
- promptFile 字段为空（依赖 MCP 工具而非 prompt 文件，如 `vocab-coach`、`gaokao-advisor`）

**补充来源**：WorkBuddy 客户端安装后会在本地缓存已使用的 plugin agent MD：

```
~/.workbuddy/plugins/marketplaces/experts/plugins/{plugin-name}/agents/
~/.workbuddy/plugins/marketplaces/cb_teams_marketplace/plugins/{plugin-name}/agents/
```

**建议**：COS 404 时，先检查本地 WorkBuddy 缓存目录是否有对应文件。

### 3. manifest categories 的 name 字段是 dict 而非 str

**现象**：脚本用 `c['name']['zh']` 取分类名时报 `TypeError: string indices must be integers`。

**根因**：新版本 manifest 的 categories 结构变了，`name` 字段从 `{"en": "...", "zh": "..."}` 变为顶层 `zh`/`en` 字段：

```json
// 旧版
{"id": "01-ProductDesign", "name": {"en": "Product Design", "zh": "产品设计"}}

// 新版
{"id": "01-ProductDesign", "zh": "产品设计", "en": "Product Design"}
```

**修复**：取分类名时兼容两种格式：

```python
for c in categories:
    if 'name' in c:
        zh = c['name']['zh'] if isinstance(c['name'], dict) else c['name']
    else:
        zh = c.get('zh', c.get('en', ''))
    cat_map[c['id']] = zh
```

### 4. WorkBuddy 的 team 和单专家是不同的

**关键区别**：
- **单专家**：1 个 plugin = 1 个 agent，manifest 中 `expertType: "agent"`，按领域存放在 `bin/agents/workbuddy/{领域}/`
- **团队**：1 个 plugin = 多个 agent，manifest 中 `expertType: "team"`，存放在 `bin/teams/{团队名}/agents/`
- 团队的 member agent 有团队协作上下文的 prompt，独立专家是通用能力
- **不要把团队 leader agent 当作独立专家加入 experts YAML**

### 5. agent MD 命名不一致

**现象**：team.yaml 中的角色名与实际 agent MD 文件名不匹配。

**典型案例**：设计原型专家团中，manifest 的 member id 为 `evidence exchange-analyst`（含空格），但 promptFile 指向 `discovery-analyst.md`。

**修复**：以实际文件名为准，更新 team.yaml 中的角色名。

### 6. yaml.dump 会破坏 experts YAML 格式

**现象**：用 `yaml.dump` 重写 experts YAML 后，icon 等字段变成空字符串，格式被标准化。

**建议**：
- 更新 experts YAML 时，先读取已有内容，只修改/新增需要变更的条目，再整体写回
- 或使用文本操作（正则替换）而非 yaml.dump，保留原始格式
- 如果必须用 yaml.dump，确保所有字段（包括 icon）都有正确的值

### 7. Go nil slice → JS null

**现象**：Go 后端返回 nil slice 时，Wails 序列化为 JS null 而非空数组，导致前端 `map is not a function` 错误。

**修复**：所有 Wails 绑定函数返回 slice 时用 `make([]T, 0)` 替代 `var result []T`。

### 8. ListSessions 性能优化

**现象**：2488 个 session 文件全量读取 meta.json 排序，导致 UI 卡顿。

**修复**：先按文件修改时间排序，再只读前 50 个 meta.json。

### 9. 增量更新流程

当 WorkBuddy 发布新版本时，增量更新步骤：

1. 读取新 manifest，对比现有 `bin/agents/workbuddy/` 中的 agent 文件
2. 下载新增的 agent MD（跳过已存在的）
3. 检查本地 WorkBuddy 缓存 `~/.workbuddy/plugins/marketplaces/` 补充 COS 404 的
4. 更新 experts YAML（只新增条目，不覆盖已有 whale 自有条目）
5. 检查团队 agents/ 是否有错位文件，清理
6. 验证：`go build ./cmd/whale` + `npm run build`（前端）

### 10. 完整性验证脚本

```python
import yaml, os

def verify_all():
    experts_dir = r'D:\src\DeepSeek-Code-Whale\bin\experts'
    agents_dir = r'D:\src\DeepSeek-Code-Whale\bin\agents'
    teams_dir = r'D:\src\DeepSeek-Code-Whale\bin\teams'

    # Build full agent index
    all_agents = {}
    for root, dirs, files in os.walk(agents_dir):
        for f in files:
            if f.endswith('.md'):
                all_agents[f[:-3]] = os.path.join(root, f)

    # Verify experts YAML
    for f in sorted(os.listdir(experts_dir)):
        if not f.endswith('.yaml'):
            continue
        with open(os.path.join(experts_dir, f), 'r', encoding='utf-8') as fh:
            data = yaml.safe_load(fh)
        for e in (data or {}).get('experts', []):
            agent = e.get('agent', '')
            agent_name = agent.split('/')[-1] if '/' in agent else agent
            if agent_name and agent_name not in all_agents:
                print(f'MISSING expert agent: {f}: {e.get("name","")} -> {agent}')

    # Verify team agents
    for team_name in sorted(os.listdir(teams_dir)):
        team_yaml = os.path.join(teams_dir, team_name, 'team.yaml')
        agents_dir_t = os.path.join(teams_dir, team_name, 'agents')
        if not os.path.exists(team_yaml):
            continue
        with open(team_yaml, 'r', encoding='utf-8') as fh:
            data = yaml.safe_load(fh)
        roles = set(data.get('roles', []))
        existing = set()
        if os.path.exists(agents_dir_t):
            existing = {fn[:-3] for fn in os.listdir(agents_dir_t) if fn.endswith('.md')}
        missing = roles - existing
        misplaced = existing - roles
        if missing:
            print(f'TEAM MISSING: {team_name}: {sorted(missing)}')
        if misplaced:
            print(f'TEAM MISPLACED: {team_name}: {sorted(misplaced)}')

verify_all()
```