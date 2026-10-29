import { builtinScriptlets } from './resources/scriptlets.js';

self.scriptletGlobals ??= {};

const m = Object.create(null);
for (const s of builtinScriptlets) {
  m[s.name.slice(0, -3)] = s.fn;
  for (const a of s.aliases ?? []) m[a.slice(0, -3)] = s.fn;
}
self.__bp = m;
