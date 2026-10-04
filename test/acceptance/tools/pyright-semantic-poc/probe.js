#!/usr/bin/env node
'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');

const EXPECTED_VERSION = '1.1.414';
const EXPECTED_HASHES = {
  'dist/pyright.js': '15e1bd7955a81f9e5210dc44da50a9baa7837780a8ac8ce14435dac325fccb50',
  'dist/pyright-internal.js': '41fb260ad72f2a16c5657e1f2fe837104d7529255d0fcd955f48432b276e7cae',
  'dist/vendor.js': '1c691bf9e3fa954dec751ace203beb5591b46a73e3848af5d1ee011d62540798',
};
const LIMITS = { fixtureFiles: 4, fixtureBytes: 32768, programFiles: 64, outputBytes: 262144 };

function fail(message) {
  throw new Error(message);
}

function sha256(filePath) {
  return crypto.createHash('sha256').update(fs.readFileSync(filePath)).digest('hex');
}

function checkPinnedBundle(pyrightRoot) {
  const pkg = JSON.parse(fs.readFileSync(path.join(pyrightRoot, 'package.json'), 'utf8'));
  if (pkg.version !== EXPECTED_VERSION) fail(`Pyright version mismatch: expected ${EXPECTED_VERSION}, got ${pkg.version}`);
  const actualHashes = {};
  for (const [relativePath, expected] of Object.entries(EXPECTED_HASHES)) {
    const actual = sha256(path.join(pyrightRoot, relativePath));
    actualHashes[relativePath] = actual;
    if (actual !== expected) fail(`Pyright bundle hash mismatch: ${relativePath} expected ${expected}, got ${actual}`);
  }
  return { version: pkg.version, hashes: actualHashes };
}

function checkFixtureBounds(fixtureRoot) {
  const entries = fs.readdirSync(fixtureRoot, { withFileTypes: true });
  if (entries.length > LIMITS.fixtureFiles) fail(`fixture count exceeds ${LIMITS.fixtureFiles}`);
  let totalBytes = 0;
  for (const entry of entries) {
    if (!entry.isFile()) fail(`unexpected fixture entry: ${entry.name}`);
    const filePath = path.join(fixtureRoot, entry.name);
    const size = fs.statSync(filePath).size;
    if (size > LIMITS.fixtureBytes) fail(`fixture too large: ${entry.name}`);
    totalBytes += size;
  }
  if (totalBytes > LIMITS.fixtureBytes) fail(`fixture bytes exceed ${LIMITS.fixtureBytes}`);
  return { count: entries.length, bytes: totalBytes };
}

function createPrivateWebpackRequire(distRoot) {
  const internal = require(path.join(distRoot, 'pyright-internal.js'));
  const vendor = require(path.join(distRoot, 'vendor.js'));
  const duplicates = Object.keys(internal.modules).filter((id) => Object.hasOwn(vendor.modules, id));
  if (duplicates.length) fail(`unexpected duplicate webpack module ids: ${duplicates.slice(0, 8).join(',')}`);
  const modules = { ...vendor.modules, ...internal.modules };
  const externalBuiltins = {
    8240: 'fsevents', 5317: 'node:child_process', 6982: 'node:crypto', 4434: 'node:events',
    9896: 'node:fs', 857: 'node:os', 6928: 'node:path', 932: 'node:process',
    3785: 'node:readline', 2203: 'node:stream', 2018: 'node:tty', 7016: 'node:url',
    9023: 'node:util', 1493: 'node:v8', 8167: 'node:worker_threads', 3106: 'node:zlib',
  };
  const cache = Object.create(null);
  function privateRequire(id) {
    if (Object.hasOwn(cache, id)) return cache[id].exports;
    const factory = modules[id];
    if (!factory) {
      if (externalBuiltins[id]) return require(externalBuiltins[id]);
      fail(`private webpack module is unavailable: ${id}`);
    }
    const module = { exports: {} };
    cache[id] = module;
    factory(module, module.exports, privateRequire);
    return module.exports;
  }
  privateRequire.m = modules;
  privateRequire.o = (object, key) => Object.prototype.hasOwnProperty.call(object, key);
  privateRequire.d = (target, definitions) => {
    for (const key of Object.keys(definitions)) {
      if (!privateRequire.o(target, key)) Object.defineProperty(target, key, { enumerable: true, get: definitions[key] });
    }
  };
  privateRequire.r = (target) => {
    if (typeof Symbol !== 'undefined' && Symbol.toStringTag) Object.defineProperty(target, Symbol.toStringTag, { value: 'Module' });
    Object.defineProperty(target, '__esModule', { value: true });
  };
  privateRequire.nmd = (module) => {
    module.paths = [];
    module.children ||= [];
    return module;
  };
  privateRequire.n = (module) => {
    const getter = module && module.__esModule ? () => module.default : () => module;
    privateRequire.d(getter, { a: getter });
    return getter;
  };
  return { privateRequire, internalModuleCount: Object.keys(internal.modules).length };
}

function walkParseNodes(root, cap = 4096, typeNames = {}) {
  const seen = new WeakSet();
  const nodes = [];
  const stack = [root];
  while (stack.length) {
    const node = stack.pop();
    if (!node || typeof node !== 'object' || !Number.isInteger(node.nodeType) || seen.has(node)) continue;
    seen.add(node);
    nodes.push(node);
    if (nodes.length > cap) fail(`parse node cap exceeded (${cap}) at ${typeNames[node.nodeType] || node.nodeType}, dKeys=${Object.keys(node.d || {}).join(',')}`);
    const data = node.d;
    if (!data || typeof data !== 'object') continue;
    for (const [key, value] of Object.entries(data)) {
      if (key === 'token') continue;
      if (Array.isArray(value)) {
        for (let i = value.length - 1; i >= 0; i--) {
          if (Number.isInteger(value[i]?.nodeType)) stack.push(value[i]);
        }
      } else if (value && Number.isInteger(value.nodeType)) {
        stack.push(value);
      }
    }
  }
  return nodes;
}

function main() {
  const repoRoot = path.resolve(__dirname, '..', '..', '..', '..');
  const pyrightRoot = path.join(repoRoot, 'test', 'acceptance', 'tools', 'node_modules', 'pyright');
  const fixtureRoot = path.join(__dirname, 'fixtures');
  const pinned = checkPinnedBundle(pyrightRoot);
  const fixture = checkFixtureBounds(fixtureRoot);
  const { privateRequire, internalModuleCount } = createPrivateWebpackRequire(path.join(pyrightRoot, 'dist'));

  const { createServiceProvider } = privateRequire(1795);
  const { Uri } = privateRequire(9424);
  const { createFromRealFileSystem, RealTempFile } = privateRequire(4788);
  const { PyrightFileSystem } = privateRequire(3761);
  const { FullAccessHost } = privateRequire(210);
  const { AnalyzerService } = privateRequire(5987);
  const { ParseNodeTypeNameMap } = privateRequire(465);
  const { getDeclaration, getImportInfo } = privateRequire(1920);

  const consoleSink = { log() {}, info() {}, warn() {}, error() {} };
  const tempFile = new RealTempFile();
  const fileSystem = new PyrightFileSystem(createFromRealFileSystem(tempFile, consoleSink));
  const serviceProvider = createServiceProvider(fileSystem, tempFile, consoleSink);
  const service = new AnalyzerService('pyright-private-poc', serviceProvider, {
    fileSystem,
    console: consoleSink,
    hostFactory: () => new FullAccessHost(serviceProvider),
  });

  try {
    service.setOptions({
      executionRoot: fixtureRoot,
      configFilePath: path.join(fixtureRoot, 'pyrightconfig.json'),
      configSettings: {
        includeFileSpecs: [],
        excludeFileSpecs: [],
        ignoreFileSpecs: [],
        diagnosticSeverityOverrides: {},
        diagnosticBooleanOverrides: {},
        includeFileSpecsOverride: [],
      },
      languageServerSettings: {},
    });
    if (!service.enumerateSourceFiles(2000)) fail('bounded fixture source enumeration did not complete');
    service.run((program) => program.analyze({ openFilesTimeInMs: 5000, noOpenFilesTimeInMs: 5000 }), undefined);

    const program = service._program;
    const uris = service.getUserFiles();
    if (uris.length > LIMITS.programFiles) fail(`Program file count exceeds ${LIMITS.programFiles}: ${uris.length}`);
    const rows = [];
    for (const uri of uris) {
      const sourceFile = service.getSourceFile(uri);
      const parsed = service.getParseResults(uri);
      const parseTree = parsed?.parserOutput?.parseTree;
      if (!parseTree) fail(`parser output lacks parseTree; result=${Object.keys(parsed || {}).join(',')}; parser=${Object.keys(parsed?.parserOutput || {}).join(',')}`);
      const nodes = walkParseNodes(parseTree, 4096, ParseNodeTypeNameMap);
      const sample = nodes
        .filter((node) => ['Name', 'Call', 'Import', 'ImportFrom'].includes(ParseNodeTypeNameMap[node.nodeType]))
        .slice(0, 80)
        .map((node) => {
          const declaration = getDeclaration(node, program.analyzerNodeInfoReader);
          const importInfo = getImportInfo(node, program.analyzerNodeInfoReader);
          return {
            kind: ParseNodeTypeNameMap[node.nodeType] || `node-${node.nodeType}`,
            start: node.start,
            length: node.length,
            dataKeys: Object.keys(node.d || {}).slice(0, 12),
            value: typeof node.d?.value === 'string' ? node.d.value : undefined,
            declaration: declaration ? Object.keys(declaration).slice(0, 12) : null,
            importInfo: importInfo ? Object.keys(importInfo).slice(0, 12) : null,
          };
        });
      rows.push({ uri: uri.toString(), parseTreeType: ParseNodeTypeNameMap[parseTree.nodeType], nodeCount: nodes.length, sample });
      if (rows.length > LIMITS.programFiles) fail(`output file count exceeds ${LIMITS.programFiles}`);
    }

    const output = {
      status: 'private-api-poc',
      pinned,
      bounds: { fixture, programFiles: uris.length, outputBytes: LIMITS.outputBytes },
      privateModuleCount: internalModuleCount,
      pyrightPublicApi: false,
      programMethods: Object.getOwnPropertyNames(Object.getPrototypeOf(program)),
      files: rows,
    };
    const json = JSON.stringify(output, null, 2);
    if (Buffer.byteLength(json, 'utf8') > LIMITS.outputBytes) fail(`output exceeds ${LIMITS.outputBytes} bytes`);
    process.stdout.write(`${json}\n`);
  } finally {
    service.dispose();
  }
}

try {
  main();
} catch (error) {
  process.stderr.write(`POC_FAILURE: ${error && error.stack ? error.stack : String(error)}\n`);
  process.exitCode = 1;
}
