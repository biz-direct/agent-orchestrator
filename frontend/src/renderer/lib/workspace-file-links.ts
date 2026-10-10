/**
 * Plain-text file references (terminal output, agent prose, tool output) that
 * can open in the session's file viewer.
 *
 * One detector and one resolver serve every surface, so a path that links in
 * the terminal links the same way in chat. Detection is deliberately loose and
 * resolution deliberately strict: a token only becomes a link when it names a
 * file that is in the session's workspace catalog and that the viewer can
 * render. The catalog is the daemon's own file list, so a link never needs a
 * per-path existence probe and never points outside the workspace.
 */

import { normalizeWorkspaceFileReference, workspaceFileReferenceLine } from "./workspace-file-path";

export type WorkspaceFileCatalog = {
	/** Workspace-relative paths that exist in the session worktree. */
	paths: readonly string[];
	/** Catalog paths the file viewer cannot render (non-image binaries). */
	unviewable?: ReadonlySet<string>;
};

/** A path-shaped token found in text, before it is checked against a workspace. */
export type FileReference = {
	start: number;
	end: number;
	/** The linked text, location suffix included. */
	text: string;
	/** The path without its location suffix. */
	path: string;
	line?: number;
	column?: number;
};

/** A reference that resolved to a viewer-openable workspace file. */
export type WorkspaceFileLink = FileReference & { workspacePath: string };

/**
 * Raster formats FileContentPane previews instead of reporting "binary".
 * Mirrors `workspaceImageMediaTypes` in backend/internal/service/session/workspace_files.go,
 * which sets the `imageMediaType` the viewer branches on.
 */
const VIEWER_IMAGE_EXTENSIONS = new Set(["apng", "avif", "bmp", "gif", "ico", "jpeg", "jpg", "png", "webp"]);

/** Whether FileContentPane can render this file rather than a "binary" placeholder. */
export function isViewerSupportedWorkspaceFile(file: { path: string; binary?: boolean }): boolean {
	if (!file.binary) return true;
	const dot = file.path.lastIndexOf(".");
	return dot >= 0 && VIEWER_IMAGE_EXTENSIONS.has(file.path.slice(dot + 1).toLowerCase());
}

/** HTML is served by the AO Browser preview rather than shown as source. */
export function workspaceFileOpensInBrowser(path: string): boolean {
	return /\.html?$/i.test(path);
}

// Characters that never belong to a path we link: whitespace, quotes, brackets,
// and list separators. Paths containing spaces are intentionally unsupported.
const TOKEN = /[^\s"'`<>()[\]{}|,;“”‘’«»]+/g;
// Markdown emphasis, `@` mentions, and prose punctuation around a path; `_`
// and `~` stay because they start real names (`__init__.py`, `~/notes.md`).
const LEADING_NOISE = /^[*@]+/;
const TRAILING_NOISE = /[.,:;!?*]+$/;
const COLON_LOCATION = /:(\d+)(?::(\d+))?$/;
const HASH_LOCATION = /#L(\d+)(?:C(\d+))?(?:-L?\d+(?:C\d+)?)?$/i;
// `file.ts(12,5)` as printed by tsc and MSBuild-style tools.
const PAREN_LOCATION = /^\((\d+)(?:,\s*(\d+))?\)/;

function positive(value: string | undefined): number | undefined {
	if (value == null) return undefined;
	const number = Number(value);
	return Number.isSafeInteger(number) && number > 0 ? number : undefined;
}

function looksLikeFilePath(path: string): boolean {
	if (!path || path.endsWith("/") || path.endsWith("\\")) return false;
	return /[\\/]/.test(path) || /[^./\\]\.[A-Za-z0-9][A-Za-z0-9_-]{0,15}$/.test(path);
}

/** Find path-shaped tokens with optional `:line[:col]`, `(line,col)`, or `#L` suffixes. */
export function findFileReferences(text: string): FileReference[] {
	const references: FileReference[] = [];
	for (const match of text.matchAll(TOKEN)) {
		let token = match[0];
		let start = match.index;
		const leading = token.match(LEADING_NOISE)?.[0].length ?? 0;
		token = token.slice(leading);
		start += leading;
		token = token.replace(TRAILING_NOISE, "");
		if (!token) continue;
		if (token.includes("://")) {
			if (!/^file:\/\//i.test(token)) continue;
		} else if (/^[A-Za-z][A-Za-z\d+.-]*:(?![\\/])/.test(token) && !COLON_LOCATION.test(token)) {
			// A scheme such as mailto: or ao: — not a path.
			continue;
		}

		let path = token;
		let line: number | undefined;
		let column: number | undefined;
		let end = start + token.length;
		const hash = path.match(HASH_LOCATION);
		const colon = hash ? null : path.match(COLON_LOCATION);
		if (hash) {
			path = path.slice(0, hash.index);
			line = positive(hash[1]);
			column = positive(hash[2]);
		} else if (colon) {
			path = path.slice(0, colon.index);
			line = positive(colon[1]);
			column = positive(colon[2]);
		} else if (end === match.index + match[0].length) {
			const paren = text.slice(end).match(PAREN_LOCATION);
			if (paren) {
				line = positive(paren[1]);
				column = positive(paren[2]);
				end += paren[0].length;
			}
		}
		if (/^file:\/\//i.test(path)) {
			try {
				path = decodeURIComponent(new URL(path).pathname);
				if (/^\/[A-Za-z]:\//.test(path)) path = path.slice(1);
			} catch {
				continue;
			}
		}
		if (!looksLikeFilePath(path)) continue;
		references.push({
			start,
			end,
			text: text.slice(start, end),
			path,
			...(line != null ? { line } : {}),
			...(column != null ? { column } : {}),
		});
	}
	return references;
}

type CatalogIndex = {
	exact: Map<string, string>;
	byBase: Map<string, string[]>;
};

const indexes = new WeakMap<readonly string[], CatalogIndex>();

function normalize(path: string): string {
	return path.trim().replace(/\\/g, "/").replace(/^(?:\.\/)+/, "");
}

function basename(path: string): string {
	return path.slice(path.lastIndexOf("/") + 1);
}

function catalogIndex(paths: readonly string[]): CatalogIndex {
	let index = indexes.get(paths);
	if (index) return index;
	index = { exact: new Map(), byBase: new Map() };
	for (const path of paths) {
		const normalized = normalize(path);
		index.exact.set(normalized, path);
		const base = basename(normalized);
		const bucket = index.byBase.get(base);
		if (bucket) bucket.push(path);
		else index.byBase.set(base, [path]);
	}
	indexes.set(paths, index);
	return index;
}

function isAbsolute(path: string): boolean {
	return path.startsWith("/") || path.startsWith("~/") || /^[A-Za-z]:\//.test(path);
}

/**
 * Resolve a displayed path to the catalog path it names, or undefined.
 *
 * Absolute paths must end with a complete workspace-relative path (the longest
 * wins), so `/elsewhere/x.ts` never matches on basename alone. Relative paths
 * match exactly, then by a unique path suffix in either direction (output
 * printed from a subdirectory, or git's `a/` and `b/` prefixes), then by a
 * unique basename.
 */
export function resolveWorkspaceFilePath(rawPath: string, catalog: WorkspaceFileCatalog): string | undefined {
	const normalized = normalize(rawPath);
	if (!normalized || normalized.split("/").some((segment) => segment === "..")) return undefined;
	const index = catalogIndex(catalog.paths);
	const viewable = (path: string | undefined) => (path && !catalog.unviewable?.has(path) ? path : undefined);

	const sameBase = index.byBase.get(basename(normalized)) ?? [];
	if (isAbsolute(normalized)) {
		const matches = sameBase
			.filter((candidate) => normalized.endsWith(`/${normalize(candidate)}`))
			.sort((left, right) => right.length - left.length);
		if (matches.length > 1 && matches[0]!.length === matches[1]!.length) return undefined;
		return viewable(matches[0]);
	}

	const exact = index.exact.get(normalized);
	if (exact) return viewable(exact);

	const suffixes = sameBase.filter((candidate) => {
		const comparable = normalize(candidate);
		return comparable.endsWith(`/${normalized}`) || normalized.endsWith(`/${comparable}`);
	});
	if (suffixes.length === 1) return viewable(suffixes[0]);
	if (suffixes.length > 1) return undefined;
	return !normalized.includes("/") && sameBase.length === 1 ? viewable(sameBase[0]) : undefined;
}

/** Find every file reference in `text` that opens in the session's file viewer. */
export function findWorkspaceFileLinks(text: string, catalog: WorkspaceFileCatalog): WorkspaceFileLink[] {
	if (catalog.paths.length === 0) return [];
	return findFileReferences(text).flatMap((reference) => {
		const workspacePath = resolveWorkspaceFilePath(reference.path, catalog);
		return workspacePath ? [{ ...reference, workspacePath }] : [];
	});
}

/**
 * Resolve an href or inline-code reference (`src/a.ts:12`, `file:///…#L4`,
 * percent-encoded markdown targets) to a viewer-openable workspace file.
 */
export function resolveWorkspaceFileReference(
	reference: string,
	catalog: WorkspaceFileCatalog,
): { workspacePath: string; line?: number } | undefined {
	const trimmed = reference.trim();
	if (!trimmed || trimmed.startsWith("#") || trimmed.startsWith("//")) return undefined;
	if (/^[A-Za-z][A-Za-z\d+.-]*:(?!\d)/.test(trimmed) && !/^file:\/\//i.test(trimmed) && !/^[A-Za-z]:[\\/]/.test(trimmed)) {
		return undefined;
	}
	const path = normalizeWorkspaceFileReference(trimmed);
	const workspacePath = path ? resolveWorkspaceFilePath(path, catalog) : undefined;
	if (!workspacePath) return undefined;
	const line = workspaceFileReferenceLine(trimmed);
	return line == null ? { workspacePath } : { workspacePath, line };
}
