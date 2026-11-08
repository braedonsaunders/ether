// TypeScript 7 (the root `typescript` dependency) is the Go-native compiler
// and ships no JavaScript compiler API. The plugin Studio's in-browser
// language service and the i18n extractor still need that API, so they use
// the TypeScript 5.9 `typescript-api` alias. The two libraries below import
// `typescript` themselves (@typescript/vfs as a peer, @valtown/codemirror-ts
// without declaring it); point both at the same 5.9 build so the bundle holds
// one compiler and the types line up.
const API = "npm:typescript@^5.9.3";
const API_CONSUMERS = new Set(["@typescript/vfs", "@valtown/codemirror-ts"]);

module.exports = {
  hooks: {
    readPackage(pkg) {
      if (API_CONSUMERS.has(pkg.name)) {
        if (pkg.peerDependencies) delete pkg.peerDependencies.typescript;
        pkg.dependencies = { ...pkg.dependencies, typescript: API };
      }
      return pkg;
    },
  },
};
