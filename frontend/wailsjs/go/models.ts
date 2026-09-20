export namespace cache {
	
	export class HealthReport {
	    // Go type: time
	    checked_at: any;
	    files: number;
	    bytes: number;
	    valid_files: number;
	    invalid_files: number;
	    invalid_bytes: number;
	    roots: string[];
	
	    static createFrom(source: any = {}) {
	        return new HealthReport(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.checked_at = this.convertValues(source["checked_at"], null);
	        this.files = source["files"];
	        this.bytes = source["bytes"];
	        this.valid_files = source["valid_files"];
	        this.invalid_files = source["invalid_files"];
	        this.invalid_bytes = source["invalid_bytes"];
	        this.roots = source["roots"];
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

}

export namespace cert {
	
	export class Status {
	    ready: boolean;
	    installed: boolean;
	    fingerprint_sha256?: string;
	    // Go type: time
	    created_at?: any;
	    directory: string;
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new Status(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ready = source["ready"];
	        this.installed = source["installed"];
	        this.fingerprint_sha256 = source["fingerprint_sha256"];
	        this.created_at = this.convertValues(source["created_at"], null);
	        this.directory = source["directory"];
	        this.error = source["error"];
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

}

export namespace logging {
	
	export class Entry {
	    // Go type: time
	    time: any;
	    category: string;
	    method?: string;
	    target?: string;
	    message?: string;
	    duration?: string;
	
	    static createFrom(source: any = {}) {
	        return new Entry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.time = this.convertValues(source["time"], null);
	        this.category = source["category"];
	        this.method = source["method"];
	        this.target = source["target"];
	        this.message = source["message"];
	        this.duration = source["duration"];
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

}

export namespace main {
	
	export class ConnectionTestResult {
	    ok: boolean;
	    host: string;
	    status: number;
	    dns_ms: number;
	    tcp_ms: number;
	    tls_ms: number;
	    ttfb_ms: number;
	    duration_ms: number;
	    download_bytes: number;
	    throughput_bps: number;
	    http_version: string;
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new ConnectionTestResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.host = source["host"];
	        this.status = source["status"];
	        this.dns_ms = source["dns_ms"];
	        this.tcp_ms = source["tcp_ms"];
	        this.tls_ms = source["tls_ms"];
	        this.ttfb_ms = source["ttfb_ms"];
	        this.duration_ms = source["duration_ms"];
	        this.download_bytes = source["download_bytes"];
	        this.throughput_bps = source["throughput_bps"];
	        this.http_version = source["http_version"];
	        this.error = source["error"];
	    }
	}

}

export namespace migration {
	
	export class Status {
	    state: string;
	    old_root?: string;
	    new_root?: string;
	    total_bytes: number;
	    copied_bytes: number;
	    total_files: number;
	    copied_files: number;
	    speed_bytes_per_second: number;
	    current_file?: string;
	    error?: string;
	    // Go type: time
	    started_at?: any;
	    // Go type: time
	    finished_at?: any;
	
	    static createFrom(source: any = {}) {
	        return new Status(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.state = source["state"];
	        this.old_root = source["old_root"];
	        this.new_root = source["new_root"];
	        this.total_bytes = source["total_bytes"];
	        this.copied_bytes = source["copied_bytes"];
	        this.total_files = source["total_files"];
	        this.copied_files = source["copied_files"];
	        this.speed_bytes_per_second = source["speed_bytes_per_second"];
	        this.current_file = source["current_file"];
	        this.error = source["error"];
	        this.started_at = this.convertValues(source["started_at"], null);
	        this.finished_at = this.convertValues(source["finished_at"], null);
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

}

export namespace service {
	
	export class Snapshot {
	    state: string;
	    listen_address: string;
	    network_mode: string;
	    cache_root: string;
	    ram_used_bytes: number;
	    ram_max_bytes: number;
	    disk_bytes: number;
	    stats: stats.Counters;
	    certificate: cert.Status;
	    migration: migration.Status;
	    logs: logging.Entry[];
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new Snapshot(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.state = source["state"];
	        this.listen_address = source["listen_address"];
	        this.network_mode = source["network_mode"];
	        this.cache_root = source["cache_root"];
	        this.ram_used_bytes = source["ram_used_bytes"];
	        this.ram_max_bytes = source["ram_max_bytes"];
	        this.disk_bytes = source["disk_bytes"];
	        this.stats = this.convertValues(source["stats"], stats.Counters);
	        this.certificate = this.convertValues(source["certificate"], cert.Status);
	        this.migration = this.convertValues(source["migration"], migration.Status);
	        this.logs = this.convertValues(source["logs"], logging.Entry);
	        this.error = source["error"];
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

}

export namespace stats {
	
	export class Counters {
	    total_requests: number;
	    ram_hits: number;
	    disk_hits: number;
	    misses: number;
	    origin_bytes: number;
	    cache_bytes_served: number;
	    bytes_saved: number;
	    errors: number;
	    range_requests: number;
	    cacheable_requests: number;
	
	    static createFrom(source: any = {}) {
	        return new Counters(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.total_requests = source["total_requests"];
	        this.ram_hits = source["ram_hits"];
	        this.disk_hits = source["disk_hits"];
	        this.misses = source["misses"];
	        this.origin_bytes = source["origin_bytes"];
	        this.cache_bytes_served = source["cache_bytes_served"];
	        this.bytes_saved = source["bytes_saved"];
	        this.errors = source["errors"];
	        this.range_requests = source["range_requests"];
	        this.cacheable_requests = source["cacheable_requests"];
	    }
	}

}

