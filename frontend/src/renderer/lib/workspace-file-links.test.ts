import { describe, expect, it } from "vitest";
import {
	findFileReferences,
	findWorkspaceFileLinks,
	isViewerSupportedWorkspaceFile,
	resolveWorkspaceFilePath,
	type WorkspaceFileCatalog,
} from "./workspace-file-links";

const catalog: WorkspaceFileCatalog = {
	paths: [
		"README.md",
		"frontend/src/App.tsx",
		"frontend/src/lib/util.ts",
		"backend/internal/lib/util.ts",
		"backend/cmd/main.go",
		"docs/logo.png",
		"docs/manual.pdf",
		"pkg/__init__.py",
	],
	unviewable: new Set(["docs/manual.pdf"]),
};

function links(text: string) {
	return findWorkspaceFileLinks(text, catalog).map(({ text, workspacePath, line, column }) => ({
		text,
		workspacePath,
		line,
		column,
	}));
}

describe("findFileReferences", () => {
	it.each([
		["frontend/src/App.tsx:42", { path: "frontend/src/App.tsx", line: 42 }],
		["frontend/src/App.tsx:42:7", { path: "frontend/src/App.tsx", line: 42, column: 7 }],
		["frontend/src/App.tsx#L42", { path: "frontend/src/App.tsx", line: 42 }],
		["frontend/src/App.tsx#L42C3-L50", { path: "frontend/src/App.tsx", line: 42, column: 3 }],
		["README.md", { path: "README.md" }],
	])("parses %s", (text, expected) => {
		expect(findFileReferences(text)).toEqual([{ start: 0, end: text.length, text, ...expected }]);
	});

	it("includes a tsc-style (line,col) suffix in the link range", () => {
		const text = "src/a.ts(12,5): error TS2322";
		expect(findFileReferences(text)[0]).toMatchObject({ text: "src/a.ts(12,5)", path: "src/a.ts", line: 12, column: 5 });
	});

	it.each([
		["\"src/a.ts\"", "src/a.ts"],
		["'src/a.ts'", "src/a.ts"],
		["(src/a.ts)", "src/a.ts"],
		["[src/a.ts:3]", "src/a.ts:3"],
		["see src/a.ts.", "src/a.ts"],
		["src/a.ts:12: error", "src/a.ts:12"],
		["**src/a.ts**", "src/a.ts"],
		["`src/a.ts`,", "src/a.ts"],
	])("strips the surrounding punctuation in %s", (text, expected) => {
		expect(findFileReferences(text).map((reference) => reference.text)).toEqual([expected]);
	});

	it("reports offsets into the original text", () => {
		const text = "M  (src/a.ts:3) and b.go";
		expect(findFileReferences(text).map(({ start, end }) => text.slice(start, end))).toEqual(["src/a.ts:3", "b.go"]);
	});

	it.each([
		"https://example.com/src/a.ts",
		"ao://sessions/project/session",
		"mailto:dev@example.com",
		"plain words only",
		"src/",
		"12:30",
	])("ignores %s", (text) => {
		expect(findFileReferences(text)).toEqual([]);
	});

	it("unwraps file:// URLs", () => {
		expect(findFileReferences("file:///repo/frontend/src/App.tsx:9")[0]).toMatchObject({
			path: "/repo/frontend/src/App.tsx",
			line: 9,
		});
	});
});

describe("resolveWorkspaceFilePath", () => {
	it("matches an exact workspace path", () => {
		expect(resolveWorkspaceFilePath("./frontend/src/App.tsx", catalog)).toBe("frontend/src/App.tsx");
	});

	it("matches a path printed from a subdirectory", () => {
		expect(resolveWorkspaceFilePath("src/App.tsx", catalog)).toBe("frontend/src/App.tsx");
	});

	it("matches git's a/ and b/ diff prefixes", () => {
		expect(resolveWorkspaceFilePath("b/backend/cmd/main.go", catalog)).toBe("backend/cmd/main.go");
	});

	it("matches a unique basename and rejects an ambiguous one", () => {
		expect(resolveWorkspaceFilePath("main.go", catalog)).toBe("backend/cmd/main.go");
		expect(resolveWorkspaceFilePath("util.ts", catalog)).toBeUndefined();
		expect(resolveWorkspaceFilePath("lib/util.ts", catalog)).toBeUndefined();
	});

	it("matches absolute paths inside the worktree by their full relative path", () => {
		expect(resolveWorkspaceFilePath("/Users/me/.ao/worktrees/p/s-1/frontend/src/lib/util.ts", catalog)).toBe(
			"frontend/src/lib/util.ts",
		);
		expect(resolveWorkspaceFilePath("C:\\work\\s-1\\backend\\cmd\\main.go", catalog)).toBe("backend/cmd/main.go");
	});

	it("rejects absolute paths that only share a basename with the workspace", () => {
		expect(resolveWorkspaceFilePath("/etc/main.go", catalog)).toBeUndefined();
		expect(resolveWorkspaceFilePath("/other/repo/src/App.tsx", catalog)).toBeUndefined();
	});

	it("rejects parent traversal and files outside the catalog", () => {
		expect(resolveWorkspaceFilePath("../frontend/src/App.tsx", catalog)).toBeUndefined();
		expect(resolveWorkspaceFilePath("frontend/src/Missing.tsx", catalog)).toBeUndefined();
	});

	it("rejects files the viewer cannot render", () => {
		expect(resolveWorkspaceFilePath("docs/manual.pdf", catalog)).toBeUndefined();
		expect(resolveWorkspaceFilePath("docs/logo.png", catalog)).toBe("docs/logo.png");
	});
});

describe("findWorkspaceFileLinks", () => {
	it("links compiler, grep, and agent output with its line", () => {
		expect(links("frontend/src/App.tsx:12:4: error: bad")).toEqual([
			{ text: "frontend/src/App.tsx:12:4", workspacePath: "frontend/src/App.tsx", line: 12, column: 4 },
		]);
		expect(links("Updated `pkg/__init__.py` and README.md.")).toEqual([
			{ text: "pkg/__init__.py", workspacePath: "pkg/__init__.py", line: undefined, column: undefined },
			{ text: "README.md", workspacePath: "README.md", line: undefined, column: undefined },
		]);
	});

	it("drops an @ mention prefix from the link", () => {
		expect(links("see @README.md")).toEqual([
			{ text: "README.md", workspacePath: "README.md", line: undefined, column: undefined },
		]);
	});

	it("leaves unknown, outside, and unsupported paths as text", () => {
		expect(links("/tmp/x.log ../secret.txt docs/manual.pdf notes.txt")).toEqual([]);
	});

	it("returns nothing for an empty catalog", () => {
		expect(findWorkspaceFileLinks("README.md", { paths: [] })).toEqual([]);
	});
});

describe("isViewerSupportedWorkspaceFile", () => {
	it("accepts text and previewable images, and rejects other binaries", () => {
		expect(isViewerSupportedWorkspaceFile({ path: "a.ts", binary: false })).toBe(true);
		expect(isViewerSupportedWorkspaceFile({ path: "a.PNG", binary: true })).toBe(true);
		expect(isViewerSupportedWorkspaceFile({ path: "a.pdf", binary: true })).toBe(false);
		expect(isViewerSupportedWorkspaceFile({ path: "Makefile", binary: true })).toBe(false);
	});
});
