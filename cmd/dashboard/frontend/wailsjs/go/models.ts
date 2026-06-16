export namespace dashboard {
	
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
	export class LogFileJSON {
	    name: string;
	    size: number;
	
	    static createFrom(source: any = {}) {
	        return new LogFileJSON(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.size = source["size"];
	    }
	}
	export class MasterTaskJSON {
	    id: string;
	    goal: string;
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
	export class TaskJSON {
	    workspace_id: string;
	    id: string;
	    title: string;
	    state: string;
	    role: string;
	    elapsed_sec: number;
	    retry_count: number;
	    max_retries: number;
	    progress_pct: number;
	
	    static createFrom(source: any = {}) {
	        return new TaskJSON(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.workspace_id = source["workspace_id"];
	        this.id = source["id"];
	        this.title = source["title"];
	        this.state = source["state"];
	        this.role = source["role"];
	        this.elapsed_sec = source["elapsed_sec"];
	        this.retry_count = source["retry_count"];
	        this.max_retries = source["max_retries"];
	        this.progress_pct = source["progress_pct"];
	    }
	}
	export class WorkspaceJSON {
	    id: string;
	    path: string;
	    label: string;
	    registered: string;
	    last_seen: string;
	    online: boolean;
	    task_count: number;
	    active_count: number;
	
	    static createFrom(source: any = {}) {
	        return new WorkspaceJSON(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.path = source["path"];
	        this.label = source["label"];
	        this.registered = source["registered"];
	        this.last_seen = source["last_seen"];
	        this.online = source["online"];
	        this.task_count = source["task_count"];
	        this.active_count = source["active_count"];
	    }
	}

}

