export namespace main {
	
	export class ExecuteActionResult {
	    success: boolean;
	    output: string;
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new ExecuteActionResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.success = source["success"];
	        this.output = source["output"];
	        this.error = source["error"];
	    }
	}
	export class MCPServerInfo {
	    name: string;
	    status: string;
	    disabled: boolean;
	    connected: boolean;
	    tools: number;
	    toolNames: string[];
	    command?: string;
	    url?: string;
	    type?: string;
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new MCPServerInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.status = source["status"];
	        this.disabled = source["disabled"];
	        this.connected = source["connected"];
	        this.tools = source["tools"];
	        this.toolNames = source["toolNames"];
	        this.command = source["command"];
	        this.url = source["url"];
	        this.type = source["type"];
	        this.error = source["error"];
	    }
	}
	export class SettingsData {
	    apiKey: string;
	    model: string;
	    temperature: number;
	    maxTokens: number;
	    theme: string;
	
	    static createFrom(source: any = {}) {
	        return new SettingsData(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.apiKey = source["apiKey"];
	        this.model = source["model"];
	        this.temperature = source["temperature"];
	        this.maxTokens = source["maxTokens"];
	        this.theme = source["theme"];
	    }
	}
	export class TaskConfirmation {
	    task_id: string;
	    task_title: string;
	    role: string;
	    content: string;
	    state: string;
	
	    static createFrom(source: any = {}) {
	        return new TaskConfirmation(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.task_id = source["task_id"];
	        this.task_title = source["task_title"];
	        this.role = source["role"];
	        this.content = source["content"];
	        this.state = source["state"];
	    }
	}
	export class TeamChatMessage {
	    from: string;
	    to: string;
	    content: string;
	    timestamp: string;
	
	    static createFrom(source: any = {}) {
	        return new TeamChatMessage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.from = source["from"];
	        this.to = source["to"];
	        this.content = source["content"];
	        this.timestamp = source["timestamp"];
	    }
	}

}

export namespace pod {
	
	export class AgentDialogueJSON {
	    role: string;
	    content: string;
	
	    static createFrom(source: any = {}) {
	        return new AgentDialogueJSON(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.role = source["role"];
	        this.content = source["content"];
	    }
	}
	export class AgentInfoJSON {
	    name: string;
	    role?: string;
	    description: string;
	    whenToUse?: string;
	    category?: string;
	    tools?: string[];
	    skills?: string[];
	
	    static createFrom(source: any = {}) {
	        return new AgentInfoJSON(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.role = source["role"];
	        this.description = source["description"];
	        this.whenToUse = source["whenToUse"];
	        this.category = source["category"];
	        this.tools = source["tools"];
	        this.skills = source["skills"];
	    }
	}
	export class ChatMessageJSON {
	    time: string;
	    from: string;
	    content: string;
	    thinking?: string;
	    durationMs?: number;
	    to?: string;
	
	    static createFrom(source: any = {}) {
	        return new ChatMessageJSON(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.time = source["time"];
	        this.from = source["from"];
	        this.content = source["content"];
	        this.thinking = source["thinking"];
	        this.durationMs = source["durationMs"];
	        this.to = source["to"];
	    }
	}
	export class MasterTaskJSON {
	    id: string;
	    goal: string;
	    agent?: string;
	    session_path?: string;
	    workspace_id: string;
	    workspace_path: string;
	    workspace_label: string;
	    status: string;
	    created_at: string;
	    task_count: number;
	    done_count: number;
	    active_count: number;
	    suspended_count: number;
	    workspace_online: boolean;
	
	    static createFrom(source: any = {}) {
	        return new MasterTaskJSON(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.goal = source["goal"];
	        this.agent = source["agent"];
	        this.session_path = source["session_path"];
	        this.workspace_id = source["workspace_id"];
	        this.workspace_path = source["workspace_path"];
	        this.workspace_label = source["workspace_label"];
	        this.status = source["status"];
	        this.created_at = source["created_at"];
	        this.task_count = source["task_count"];
	        this.done_count = source["done_count"];
	        this.active_count = source["active_count"];
	        this.suspended_count = source["suspended_count"];
	        this.workspace_online = source["workspace_online"];
	    }
	}
	export class SubtaskJSON {
	    id: string;
	    title: string;
	    description: string;
	    output: string;
	    role: string;
	    state: string;
	    progress: number;
	    created_at: string;
	    parent_ids: string[];
	    batch_id: string;
	    retry_count: number;
	    max_retries: number;
	    children?: SubtaskJSON[];
	
	    static createFrom(source: any = {}) {
	        return new SubtaskJSON(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.title = source["title"];
	        this.description = source["description"];
	        this.output = source["output"];
	        this.role = source["role"];
	        this.state = source["state"];
	        this.progress = source["progress"];
	        this.created_at = source["created_at"];
	        this.parent_ids = source["parent_ids"];
	        this.batch_id = source["batch_id"];
	        this.retry_count = source["retry_count"];
	        this.max_retries = source["max_retries"];
	        this.children = this.convertValues(source["children"], SubtaskJSON);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SummonedItemJSON {
	    type: string;
	    name: string;
	    label: string;
	    category?: string;
	    description?: string;
	
	    static createFrom(source: any = {}) {
	        return new SummonedItemJSON(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.type = source["type"];
	        this.name = source["name"];
	        this.label = source["label"];
	        this.category = source["category"];
	        this.description = source["description"];
	    }
	}
	export class TeamDetailJSON {
	    name: string;
	    label: string;
	    category?: string;
	    description: string;
	    roles: string[];
	
	    static createFrom(source: any = {}) {
	        return new TeamDetailJSON(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.label = source["label"];
	        this.category = source["category"];
	        this.description = source["description"];
	        this.roles = source["roles"];
	    }
	}

}

