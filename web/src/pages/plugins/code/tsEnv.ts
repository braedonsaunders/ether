/**
 * One in-browser TypeScript environment for the plugin editor, with the ES
 * libs and the Ether SDK typings, so both editors share completion, hover
 * docs and diagnostics. Runs on the main thread in the prototype; the real
 * build moves it to a web worker.
 */
import { createSystem, createVirtualTypeScriptEnvironment } from "@typescript/vfs";
import ts from "typescript-api";
import es5 from "typescript-api/lib/lib.es5.d.ts?raw";
import es2015Core from "typescript-api/lib/lib.es2015.core.d.ts?raw";
import es2015Collection from "typescript-api/lib/lib.es2015.collection.d.ts?raw";
import es2015Iterable from "typescript-api/lib/lib.es2015.iterable.d.ts?raw";
import es2015Promise from "typescript-api/lib/lib.es2015.promise.d.ts?raw";
import es2015Symbol from "typescript-api/lib/lib.es2015.symbol.d.ts?raw";
import es2015Wellknown from "typescript-api/lib/lib.es2015.symbol.wellknown.d.ts?raw";
import { HOST_DTS, SDK_DTS } from "./sdk";

export const PLUGIN_PATH = "/plugin.ts";

let env: ReturnType<typeof createVirtualTypeScriptEnvironment> | null = null;

export function pluginEnv(source: string) {
  if (env) return env;
  const files: Record<string, string> = {
    "/lib.es5.d.ts": es5,
    "/lib.es2015.core.d.ts": es2015Core,
    "/lib.es2015.collection.d.ts": es2015Collection,
    "/lib.es2015.iterable.d.ts": es2015Iterable,
    "/lib.es2015.promise.d.ts": es2015Promise,
    "/lib.es2015.symbol.d.ts": es2015Symbol,
    "/lib.es2015.symbol.wellknown.d.ts": es2015Wellknown,
    "/ether.d.ts": SDK_DTS,
    "/ether-host.d.ts": HOST_DTS,
    [PLUGIN_PATH]: source,
  };
  const fsMap = new Map(Object.entries(files));
  const system = createSystem(fsMap);
  env = createVirtualTypeScriptEnvironment(system, [...fsMap.keys()], ts, {
    target: ts.ScriptTarget.ES2022,
    module: ts.ModuleKind.ESNext,
    moduleResolution: ts.ModuleResolutionKind.Bundler,
    strict: true,
    noLib: true,
  });
  return env;
}
