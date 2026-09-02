import type {
    Path,
    SourceFile,
} from "../ast/index.ts";
import type { SnapshotChanges } from "./proto.ts";

/**
 * A cached source file entry, identified by content hash.
 */
export interface CachedSourceFile {
    /** The cached source file object */
    file: SourceFile;
    /** The content hash from the server */
    contentHash: string;
    /** The parse options key that was used to create this file */
    parseOptionsKey: string;
    /** How many (snapshot, project) pairs resolved a path to this entry */
    refCount: number;
}

/**
 * Client-side cache for source files keyed by (path, parseOptionsKey, contentHash).
 *
 * Supports multiple versions of the same file at the same path (e.g., from
 * different snapshots with different file contents). Each version is identified
 * by its content hash and parse options key.
 *
 * What each (snapshot, project) pair resolved a path to is held in a scope of its
 * own, and an entry lives as long as some scope points at it. When a snapshot is
 * disposed, its scopes are dropped and entries nothing else points at are evicted.
 *
 * A new snapshot inherits the scopes of the one before it, minus the files the
 * server reported changed. That inheritance is recorded rather than carried out —
 * see {@link retainForSnapshot} — because the usual next thing to happen is that the
 * previous snapshot is released, and then the scope can be handed over whole rather
 * than copied a file at a time.
 */
export class SourceFileCache {
    /** Map from path to all cached versions of that file */
    private cache: Map<Path, CachedSourceFile[]> = new Map();
    /** Map from snapshotId to (projectId → what that pair resolved each path to) */
    private scopes: Map<number, Map<string, Scope>> = new Map();
    /** The one retain whose scopes have not been settled yet, if any */
    private pending: PendingRetain | undefined;

    /**
     * Get a cached source file already retained for the given (snapshot, project) pair.
     * This does not require a content hash or parse options key — it returns the entry
     * if the pair resolved this path to one. Used to skip the server request entirely
     * when retainForSnapshot has already carried the entry over.
     */
    getRetained(path: Path, snapshotId: number, projectId: string): SourceFile | undefined {
        const own = this.scopes.get(snapshotId)?.get(projectId)?.get(path);
        if (own !== undefined) return own.file;
        // this snapshot may not have taken the previous one's scopes over yet, in
        // which case its answers are still that snapshot's, minus what changed
        const pending = this.pending;
        if (pending === undefined || pending.snapshotId !== snapshotId) return undefined;
        if (pending.removedProjects.has(projectId)) return undefined;
        if (pending.invalidPaths.get(projectId)?.has(path)) return undefined;
        return this.scopes.get(pending.previousSnapshotId)?.get(projectId)?.get(path)?.file;
    }

    /**
     * Store a source file in the cache and retain it for the given (snapshot, project) pair.
     * Returns the cached file — which may be an existing entry if the hash matches.
     */
    set(path: Path, file: SourceFile, parseOptionsKey: string, contentHash: string, snapshotId: number, projectId: string): SourceFile {
        let entries = this.cache.get(path);
        if (entries === undefined) {
            entries = [];
            this.cache.set(path, entries);
        }
        // Check if we already have this exact version
        let entry = entries.find(e => e.parseOptionsKey === parseOptionsKey && e.contentHash === contentHash);
        if (entry === undefined) {
            entry = { file, contentHash, parseOptionsKey, refCount: 0 };
            entries.push(entry);
        }
        this.retain(path, entry, snapshotId, projectId);
        return entry.file;
    }

    /**
     * Retains the entry this path already has for the server's hash and parse options key,
     * if it has one, without a file to fall back on.
     *
     * This is {@link set} with the tree left out, and it decides on exactly what
     * {@link set} decides on: the same two values, read off the same source file the
     * program would have encoded, compared with the same predicate. So it hands back the
     * object {@link set} would have handed back, and answers `undefined` in precisely the
     * cases where {@link set} would have kept the tree it was given — which is when the
     * caller has to go and fetch one.
     *
     * What it buys is that the tree does not have to be fetched to be compared with. Most
     * often the entry it finds is the one {@link offer} left: a client that parsed the
     * text itself and then asked the program something about it.
     */
    retainMatching(path: Path, parseOptionsKey: string, contentHash: string, snapshotId: number, projectId: string): SourceFile | undefined {
        const entry = this.cache.get(path)?.find(e => e.parseOptionsKey === parseOptionsKey && e.contentHash === contentHash);
        if (entry === undefined) return undefined;
        this.retain(path, entry, snapshotId, projectId);
        return entry.file;
    }

    /**
     * Offers a source file the client parsed itself as the cache's copy for a path,
     * without retaining it for any (snapshot, project) pair.
     *
     * This is how a tree from `API#parseSourceFile` becomes the *same object* the
     * program later answers with. It has to be the same object: a caller that holds a
     * node from one tree and a node from another for the same path holds two nodes that
     * are equal in every way except identity, and identity is what a client keying its
     * own bookkeeping off nodes has to rely on.
     *
     * Offered rather than retained because nothing here knows the program took the file
     * at that text — the client wrote it, the compiler has not been asked yet, and its
     * parse options are the ones the *previous* snapshot would have given it. So the
     * server is still asked, and it is the server's own hash and parse options key that
     * decide: {@link set} hands back this entry when they match what came over the wire,
     * and ignores it when they do not. A wrong offer costs nothing but the entry.
     *
     * What the server is asked for is the two values rather than the whole file — see
     * {@link retainMatching}, which applies the same predicate to the same two values from
     * the same source file. The judgement is unchanged; only the tree that used to be
     * decoded alongside it and thrown away is gone.
     *
     * At most one un-retained offer is kept per path — a later one replaces it — so an
     * editing loop that never reads a file back through a program does not accumulate a
     * version per edit.
     */
    offer(path: Path, file: SourceFile, parseOptionsKey: string, contentHash: string): void {
        let entries = this.cache.get(path);
        if (entries === undefined) {
            entries = [];
            this.cache.set(path, entries);
        }
        if (entries.some(e => e.parseOptionsKey === parseOptionsKey && e.contentHash === contentHash)) return;
        const previousOffer = entries.findIndex(e => e.refCount === 0);
        if (previousOffer >= 0) entries.splice(previousOffer, 1);
        entries.push({ file, contentHash, parseOptionsKey, refCount: 0 });
    }

    /**
     * Retain cache entries from a previous snapshot for a new snapshot.
     * For each project in the previous snapshot:
     *   - Removed projects: retain nothing.
     *   - Changed projects: retain everything but the files in changedFiles/deletedFiles.
     *   - Unchanged projects: retain everything.
     *
     * Only the changes are read here. Which of the two ways that inheritance is
     * settled — handed over or copied — depends on what becomes of the previous
     * snapshot next, so this records it and {@link releaseSnapshot} settles it.
     */
    retainForSnapshot(newSnapshotId: number, previousSnapshotId: number, changes: SnapshotChanges | undefined): void {
        this.flushPending();
        if (!this.scopes.has(previousSnapshotId)) return;

        const invalidPaths = new Map<string, Set<string>>();
        const changedProjects = changes?.changedProjects;
        if (changedProjects !== undefined) {
            for (const projectId of Object.keys(changedProjects)) {
                const projectChanges = changedProjects[projectId];
                // a project listed without details has nothing to invalidate
                if (projectChanges == null) continue;
                const paths = new Set<string>();
                for (const path of projectChanges.changedFiles ?? []) paths.add(path);
                for (const path of projectChanges.deletedFiles ?? []) paths.add(path);
                invalidPaths.set(projectId, paths);
            }
        }
        this.pending = {
            snapshotId: newSnapshotId,
            previousSnapshotId,
            removedProjects: new Set(changes?.removedProjects ?? []),
            invalidPaths,
        };
    }

    /**
     * Release everything the given snapshot retained across all projects.
     * Entries with no remaining references are evicted.
     */
    releaseSnapshot(snapshotId: number): void {
        const pending = this.pending;
        if (pending !== undefined) {
            if (pending.snapshotId === snapshotId) {
                // nothing has been carried over to it yet, so there is nothing to undo
                this.pending = undefined;
            }
            else if (pending.previousSnapshotId === snapshotId) {
                // the snapshot being released is the one the next was to inherit from,
                // so hand its scopes over rather than copying them and dropping the
                // originals — unless the next snapshot has scopes of its own already,
                // which are what a copy would have had to leave alone
                if (!this.scopes.has(pending.snapshotId)) {
                    this.handOverPending(pending);
                    return;
                }
                this.flushPending();
            }
            // releasing any other snapshot cannot evict anything a pending retain
            // stands to inherit: all of that is held by the snapshot it inherits
            // from, which is not the one being released here
        }

        const projects = this.scopes.get(snapshotId);
        if (projects === undefined) return;
        for (const scope of projects.values()) {
            for (const [path, entry] of scope) this.releaseEntry(path, entry);
        }
        this.scopes.delete(snapshotId);
    }

    /**
     * Clear all entries from the cache.
     */
    clear(): void {
        this.cache.clear();
        this.scopes.clear();
        this.pending = undefined;
    }

    /**
     * Get the number of unique paths in the cache.
     */
    get size(): number {
        return this.cache.size;
    }

    /**
     * Check if a path is in the cache.
     */
    has(path: Path): boolean {
        return this.cache.has(path);
    }

    /**
     * Gives the previous snapshot's scopes to the new one outright, which is what the
     * inheritance comes to when nothing else can read them: the new snapshot is the
     * only one left that answers from them, and the paths it may not answer from are
     * exactly the ones the server reported changed.
     */
    private handOverPending(pending: PendingRetain): void {
        const projects = this.scopes.get(pending.previousSnapshotId)!;
        for (const [projectId, scope] of projects) {
            if (pending.removedProjects.has(projectId)) {
                for (const [path, entry] of scope) this.releaseEntry(path, entry);
                projects.delete(projectId);
                continue;
            }
            const invalid = pending.invalidPaths.get(projectId);
            if (invalid === undefined) continue;
            for (const path of invalid) {
                const entry = scope.get(path as Path);
                if (entry === undefined) continue;
                scope.delete(path as Path);
                this.releaseEntry(path as Path, entry);
            }
        }
        this.scopes.delete(pending.previousSnapshotId);
        this.scopes.set(pending.snapshotId, projects);
        this.pending = undefined;
    }

    /**
     * Copies what the new snapshot stood to inherit, which is what the inheritance
     * comes to when both snapshots are going to go on being read.
     */
    private flushPending(): void {
        const pending = this.pending;
        if (pending === undefined) return;
        this.pending = undefined;
        const projects = this.scopes.get(pending.previousSnapshotId);
        if (projects === undefined) return;
        for (const [projectId, scope] of projects) {
            if (pending.removedProjects.has(projectId)) continue;
            const invalid = pending.invalidPaths.get(projectId);
            let target: Scope | undefined;
            for (const [path, entry] of scope) {
                if (invalid?.has(path)) continue;
                target ??= this.scopeFor(pending.snapshotId, projectId);
                // a path the new snapshot fetched for itself already has the answer
                // it is going to keep
                if (target.has(path)) continue;
                target.set(path, entry);
                entry.refCount++;
            }
        }
    }

    /** Makes an entry what the given (snapshot, project) pair answers this path with. */
    private retain(path: Path, entry: CachedSourceFile, snapshotId: number, projectId: string): void {
        const scope = this.scopeFor(snapshotId, projectId);
        const displaced = scope.get(path);
        if (displaced === entry) return;
        scope.set(path, entry);
        entry.refCount++;
        if (displaced !== undefined) this.releaseEntry(path, displaced);
    }

    private releaseEntry(path: Path, entry: CachedSourceFile): void {
        if (--entry.refCount > 0) return;
        const entries = this.cache.get(path);
        if (entries === undefined) return;
        const index = entries.indexOf(entry);
        if (index >= 0) entries.splice(index, 1);
        if (entries.length === 0) this.cache.delete(path);
    }

    /** What a (snapshot, project) pair resolved each path it asked for to, created if new. */
    private scopeFor(snapshotId: number, projectId: string): Scope {
        let projects = this.scopes.get(snapshotId);
        if (projects === undefined) {
            projects = new Map();
            this.scopes.set(snapshotId, projects);
        }
        let scope = projects.get(projectId);
        if (scope === undefined) {
            scope = new Map();
            projects.set(projectId, scope);
        }
        return scope;
    }
}

/** What one (snapshot, project) pair resolved each path it asked for to. */
type Scope = Map<Path, CachedSourceFile>;

/** A snapshot's inheritance from the one before it, before it has been settled. */
interface PendingRetain {
    snapshotId: number;
    previousSnapshotId: number;
    /** Projects the new snapshot does not have, whose scopes it inherits nothing from */
    removedProjects: Set<string>;
    /** Per project, the paths the previous snapshot's answers no longer apply to */
    invalidPaths: Map<string, Set<string>>;
}
