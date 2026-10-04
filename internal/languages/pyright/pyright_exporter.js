'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');

const VERSION = '1.1.414';
const DEFAULT_CONFIG_SENTINEL = 'omnilsp-pyright-default-config-v1';
const INTERNAL_SHA256 = '41fb260ad72f2a16c5657e1f2fe837104d7529255d0fcd955f48432b276e7cae';
const VENDOR_SHA256 = '1c691bf9e3fa954dec751ace203beb5591b46a73e3848af5d1ee011d62540798';
const FACTS = [
  'symbol', 'declaration', 'definition', 'reference', 'implementation',
  'type_relation', 'call', 'import', 'include', 'module', 'generated_source',
];
const BATCH_LIMIT = 256;
const MAX_FILES = 4096;
const MAX_FILE_BYTES = 16 * 1024 * 1024;
const MAX_SOURCE_BYTES = 128 * 1024 * 1024;
const MAX_PARSE_NODES = 200000;
const MAX_TOTAL_NODES = 1000000;
const MAX_FACTS = 500000;
const MAX_FACT_BYTES = 128 * 1024 * 1024;
const DYNAMIC_CALLS = new Set([
  'eval', 'exec', 'compile', 'globals', 'locals', 'vars', 'getattr', 'setattr',
  'delattr', '__import__', 'import_module',
]);
const BUILTIN_NAMES = new Set([
  'abs', 'all', 'any', 'ascii', 'bin', 'bool', 'breakpoint', 'bytearray', 'bytes',
  'callable', 'chr', 'classmethod', 'compile', 'complex', 'dict', 'dir', 'divmod',
  'enumerate', 'filter', 'float', 'format', 'frozenset', 'getattr', 'hasattr',
  'hash', 'help', 'hex', 'id', 'input', 'int', 'isinstance', 'issubclass', 'iter',
  'len', 'list', 'map', 'max', 'memoryview', 'min', 'next', 'object', 'oct', 'open',
  'ord', 'pow', 'print', 'property', 'range', 'repr', 'reversed', 'round', 'set',
  'setattr', 'slice', 'sorted', 'staticmethod', 'str', 'sum', 'super', 'tuple',
  'type', 'vars', 'zip', 'NotImplemented', 'None', 'True', 'False', 'ValueError',
  'TypeError', 'RuntimeError', 'Exception', 'BaseException', 'AssertionError',
]);

function fail(message) {
  throw new Error(message);
}

function sha256(filePath) {
  return crypto.createHash('sha256').update(fs.readFileSync(filePath)).digest('hex');
}

function pathKey(value) {
  const result = path.resolve(value);
  return process.platform === 'win32' ? result.toLowerCase() : result;
}

function within(root, candidate) {
  const relative = path.relative(root, candidate);
  return relative === '' || (relative !== '..' && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative));
}

function createPrivateWebpackRequire(internalPath, vendorPath) {
  const internal = require(internalPath);
  const vendor = require(vendorPath);
  const duplicates = Object.keys(internal.modules).filter((id) => Object.hasOwn(vendor.modules, id));
  if (duplicates.length) fail(`unexpected duplicate Pyright webpack module ids: ${duplicates.slice(0, 8).join(',')}`);
  const modules = { ...vendor.modules, ...internal.modules };
  const builtins = {
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
      if (builtins[id]) return require(builtins[id]);
      fail(`pinned Pyright private module ${id} is unavailable`);
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
  return privateRequire;
}

function walkParseNodes(root, typeNames, cap) {
  const seen = new WeakSet();
  const nodes = [];
  const stack = [root];
  while (stack.length) {
    const node = stack.pop();
    if (!node || typeof node !== 'object' || !Number.isInteger(node.nodeType) || seen.has(node)) continue;
    seen.add(node);
    nodes.push(node);
    if (nodes.length > cap) fail(`Pyright parse tree exceeds the ${cap} node limit`);
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

function rangeAt(file, start, end) {
  if (!Number.isInteger(start) || !Number.isInteger(end) || start < 0 || end < start || end > file.text.length) return null;
  const position = (offset) => {
    let low = 0;
    let high = file.lineStarts.length;
    while (low + 1 < high) {
      const middle = (low + high) >> 1;
      if (file.lineStarts[middle] <= offset) low = middle;
      else high = middle;
    }
    return { line: low, character: offset - file.lineStarts[low] };
  };
  return { StartLine: position(start).line, StartChar: position(start).character, EndLine: position(end).line, EndChar: position(end).character };
}

function validCompilerRange(value) {
  return value && Number.isInteger(value.start?.line) && Number.isInteger(value.start?.character) &&
    Number.isInteger(value.end?.line) && Number.isInteger(value.end?.character) &&
    value.start.line >= 0 && value.start.character >= 0 && value.end.line >= value.start.line &&
    (value.end.line !== value.start.line || value.end.character >= value.start.character);
}

function run(request) {
  if (!request || typeof request !== 'object') fail('missing Pyright exporter request');
  const rootPath = path.resolve(request.rootPath || '');
  const compilerPath = path.resolve(request.compilerPath || '');
  const vendorPath = path.resolve(request.vendorPath || '');
  const configPath = request.configPath ? path.resolve(request.configPath) : '';
  if (!path.isAbsolute(request.rootPath) || (configPath && !within(rootPath, configPath)) || !fs.statSync(rootPath).isDirectory()) {
    fail('invalid immutable Pyright project root');
  }
  const rootReal = fs.realpathSync(rootPath);
  let configReal = '';
  const expectedConfigDigest = String(request.configDigest || '').replace(/^sha256:/i, '').toLowerCase();
  if (configPath) {
    configReal = fs.realpathSync(configPath);
    if (!within(rootReal, configReal)) fail('Pyright config resolves outside the immutable root');
    fs.readFileSync(configReal);
    if (sha256(configReal) !== expectedConfigDigest) fail('immutable Pyright config digest mismatch');
  } else {
    const defaultDigest = crypto.createHash('sha256').update(DEFAULT_CONFIG_SENTINEL).digest('hex');
    if (defaultDigest !== expectedConfigDigest) fail('default Pyright config digest mismatch');
  }
  const pyrightRoot = path.resolve(path.dirname(compilerPath), '..');
  const packageJson = JSON.parse(fs.readFileSync(path.join(pyrightRoot, 'package.json'), 'utf8'));
  if (packageJson.version !== VERSION) fail(`Pyright package version ${packageJson.version} does not match ${VERSION}`);
  if (sha256(compilerPath) !== INTERNAL_SHA256 || sha256(vendorPath) !== VENDOR_SHA256) {
    fail('Pyright analyzer bundles do not match the audited 1.1.414 hashes');
  }
  if (!request.buildContext || !request.scopeID) fail('missing Pyright scope identity');
  if (!Array.isArray(request.files) || request.files.length === 0 || request.files.length > MAX_FILES) {
    fail(`Python file count must be between 1 and ${MAX_FILES}`);
  }
  if (request.projectFiles !== undefined && (!Array.isArray(request.projectFiles) || request.projectFiles.length > MAX_FILES)) {
    fail(`nested Python file count must be between 0 and ${MAX_FILES}`);
  }

  const filesByPath = new Map();
  const filesByURI = new Map();
  const scopeFilesByPath = new Map();
  let sourceBytes = 0;
  const addManifestFiles = (manifest, scopeID, buildContext, isCurrentScope) => {
    for (const file of manifest) {
      if (!file || typeof file.uri !== 'string' || !file.uri || typeof file.path !== 'string' || !path.isAbsolute(file.path)) {
        fail('invalid immutable Python file manifest entry');
      }
      if (!file.scopeID || !file.buildContext ||
          (scopeID && file.scopeID !== scopeID) || (buildContext && file.buildContext !== buildContext)) {
        fail(`invalid Python source ownership metadata: ${file.uri}`);
      }
      const extension = path.extname(file.path).toLowerCase();
      if (extension !== '.py' && extension !== '.pyi') continue;
      const absolute = path.resolve(file.path);
      const real = fs.realpathSync(absolute);
      if (!within(rootReal, real)) fail(`Python source escapes immutable root: ${file.uri}`);
      const stat = fs.statSync(real);
      if (!stat.isFile() || stat.size > MAX_FILE_BYTES) fail(`Python source exceeds the ${MAX_FILE_BYTES} byte limit: ${file.uri}`);
      sourceBytes += stat.size;
      if (sourceBytes > MAX_SOURCE_BYTES) fail('Pyright project and nested source files exceed the 128 MiB source-text limit');
      const key = pathKey(real);
      if (filesByPath.has(key) || filesByURI.has(file.uri)) fail(`duplicate Python source in immutable manifest: ${file.uri}`);
      const text = fs.readFileSync(real, 'utf8');
      if (Buffer.byteLength(text, 'utf8') !== stat.size) fail(`Python source is not valid UTF-8: ${file.uri}`);
      const lineStarts = [0];
      for (let i = 0; i < text.length; i++) {
        const code = text.charCodeAt(i);
        if (code === 13) {
          if (text.charCodeAt(i + 1) === 10) i++;
          lineStarts.push(i + 1);
        } else if (code === 10) {
          lineStarts.push(i + 1);
        }
      }
      const record = { ...file, path: real, key, text, lineStarts, nodes: null, parseTree: null, pyrightURI: null };
      filesByPath.set(key, record);
      filesByURI.set(file.uri, record);
      if (isCurrentScope) scopeFilesByPath.set(key, record);
    }
  };
  addManifestFiles(request.files, request.scopeID, request.buildContext, true);
  addManifestFiles(request.projectFiles || [], null, null, false);
  if (!scopeFilesByPath.size) fail('immutable scope contains no .py or .pyi source files');

  const privateRequire = createPrivateWebpackRequire(compilerPath, vendorPath);
  const { createServiceProvider } = privateRequire(1795);
  const { Uri } = privateRequire(9424);
  const { createFromRealFileSystem, RealTempFile } = privateRequire(4788);
  const { PyrightFileSystem } = privateRequire(3761);
  const { FullAccessHost } = privateRequire(210);
  const { AnalyzerService } = privateRequire(5987);
  const { ParseNodeTypeNameMap } = privateRequire(465);
  const { getDeclaration } = privateRequire(1920);
  const sourceEnumerationErrors = [];
  const consoleSink = {
    log() {}, info() {}, warn() {},
    error(...values) {
      const message = values.map((value) => String(value)).join(' ');
      if (/^File or directory .+ does not exist\.$/.test(message)) sourceEnumerationErrors.push(message);
    },
  };
  const tempFile = new RealTempFile();
  const fileSystem = new PyrightFileSystem(createFromRealFileSystem(tempFile, consoleSink));
  const serviceProvider = createServiceProvider(fileSystem, tempFile, consoleSink);
  const service = new AnalyzerService('omnilsp-semantic-index', serviceProvider, {
    fileSystem,
    console: consoleSink,
    hostFactory: () => new FullAccessHost(serviceProvider),
  });

  try {
    service.setOptions({
      executionRoot: rootPath,
      configFilePath: configReal || undefined,
      configSettings: {
        includeFileSpecs: [],
        excludeFileSpecs: [],
        ignoreFileSpecs: [],
        diagnosticSeverityOverrides: {},
        diagnosticBooleanOverrides: {},
        pythonVersion: request.pythonVersion || undefined,
        pythonPlatform: request.pythonPlatform || undefined,
        stubPath: request.stubPath && request.stubPath !== 'none'
          ? path.resolve(rootPath, request.stubPath)
          : path.join(rootPath, 'typings'),
      },
      languageServerSettings: {
        pythonPath: request.pythonPath,
        extraPaths: (request.includePaths || []).map((item) => path.resolve(rootPath, item)),
      },
    });
    if (!service.enumerateSourceFiles(30000)) fail('bounded Pyright project source enumeration did not complete');
    if (sourceEnumerationErrors.length) {
      fail(`Pyright project include configuration did not resolve: ${sourceEnumerationErrors.join('; ')}`);
    }
    service.run((program) => program.analyze({ openFilesTimeInMs: 30000, noOpenFilesTimeInMs: 30000 }), undefined);
    const program = service._program;
    if (!program || !program.analyzerNodeInfoReader || !program._evaluator) fail('pinned Pyright analyzer lacks semantic program APIs');
    const reader = program.analyzerNodeInfoReader;
    const programURIs = service.getUserFiles();
    const programByPath = new Map();
    for (const uri of programURIs) {
      const userPath = uri.getFilePath();
      if (!within(rootReal, userPath)) continue;
      const key = pathKey(userPath);
      const file = filesByPath.get(key);
      if (!file) fail(`Pyright selected a project source missing from the immutable manifest: ${uri.toUserVisibleString()}`);
      file.pyrightURI = uri;
      programByPath.set(key, file);
    }
    const scopeManifestFiles = Array.from(scopeFilesByPath.values());
    const analyzedScopeFiles = new Map();
    for (const file of scopeManifestFiles) {
      if (!file.pyrightURI) file.pyrightURI = Uri.file(file.path, serviceProvider, true);
      const source = program.getSourceFile(file.pyrightURI);
      if (!programByPath.has(file.key) && !source) continue;
      if (!source) fail(`Pyright selected or loaded an immutable source without a source-file model: ${file.uri}`);
      const parsed = program.getParseResults(file.pyrightURI);
      const tree = parsed?.parserOutput?.parseTree;
      if (!tree) fail(`Pyright has no parse tree for immutable source ${file.uri}`);
      file.parseTree = tree;
      file.nodes = walkParseNodes(tree, ParseNodeTypeNameMap, MAX_PARSE_NODES);
      const parseDiagnostics = source?.getParseDiagnostics?.() || parsed?.parserOutput?.parseDiagnostics || [];
      if (parseDiagnostics.length) file.parseReason = 'Pyright reported syntax errors in an immutable Python source';
      analyzedScopeFiles.set(file.key, file);
    }
    scopeFilesByPath.clear();
    for (const [key, file] of analyzedScopeFiles) {
      scopeFilesByPath.set(key, file);
    }

    const pending = { Symbols: [], Occurrences: [], Edges: [] };
    const seenSymbols = new Set();
    const seenOccurrences = new Set();
    const seenEdges = new Set();
    let pendingCount = 0;
    let exportedCount = 0;
    let exportedBytes = 0;
    let totalNodes = 0;
    const factIncomplete = Object.create(null);
    const markIncomplete = (facts, reason) => {
      for (const fact of facts) factIncomplete[fact] = factIncomplete[fact] || reason;
    };
    const dynamicFacts = ['symbol', 'declaration', 'definition', 'reference', 'implementation', 'type_relation', 'call', 'import', 'module'];
    const coreBindingFacts = ['symbol', 'declaration', 'definition', 'reference'];
    const idByDeclaration = new Map();
    const bindingByDeclaration = new Map();
    const idByCompilerSymbol = new WeakMap();
    const fileByDeclarationPath = (declaration) => {
      const target = declaration?.uri;
      const targetPath = target?.getFilePath?.();
      if (!targetPath) return null;
      return filesByPath.get(pathKey(targetPath)) || null;
    };
    const declarationName = (declaration, fallback = '') => {
      const node = declaration?.node;
      const nameNode = node?.d?.name;
      if (typeof nameNode?.d?.value === 'string') return nameNode.d.value;
      if (typeof node?.d?.value === 'string') return node.d.value;
      if (typeof declaration?.name?.d?.value === 'string') return declaration.name.d.value;
      return fallback;
    };
    const declarationKind = (declaration) => {
      const nodeName = ParseNodeTypeNameMap[declaration?.node?.nodeType] || '';
      if (nodeName === 'Class') return 'class';
      if (nodeName === 'Function') return ParseNodeTypeNameMap[declaration.node.parent?.nodeType] === 'Class' ? 'method' : 'function';
      if (nodeName === 'Parameter') return 'parameter';
      if (nodeName === 'TypeAlias') return 'type';
      if (nodeName === 'Property') return 'property';
      return 'variable';
    };
    const compilerRange = (file, declaration) => {
      if (!file || !declaration) return null;
      const direct = declaration.range;
      if (validCompilerRange(direct) &&
          (direct.start.line !== direct.end.line || direct.start.character !== direct.end.character)) return direct;
      const node = declaration.node;
      const nameNode = ParseNodeTypeNameMap[node?.nodeType] === 'Name' ? node : node?.d?.name;
      if (!nameNode || !Number.isInteger(nameNode.start) || !Number.isInteger(nameNode.length) || nameNode.length <= 0) return null;
      const range = rangeAt(file, nameNode.start, nameNode.start + nameNode.length);
      return range && {
        start: { line: range.StartLine, character: range.StartChar },
        end: { line: range.EndLine, character: range.EndChar },
      };
    };
    const declarationKey = (declaration) => {
      const file = fileByDeclarationPath(declaration);
      const range = compilerRange(file, declaration);
      if (!file || !validCompilerRange(range)) return '';
      return `${file.uri}#${range.start.line}:${range.start.character}-${range.end.line}:${range.end.character}`;
    };
    const compilerDeclarations = (symbol) => symbol?.getDeclarations?.() || [];
    const bindingIDFromDeclarations = (rawDeclarations, fallbackName = '', compilerSymbol = null) => {
      const declarations = rawDeclarations.filter((declaration) => {
        return declaration && declaration.type !== 8 && ParseNodeTypeNameMap[declaration.node?.nodeType] !== 'Module';
      });
      if (!declarations.length) return '';
      const anchors = [];
      for (const declaration of declarations) {
        const file = fileByDeclarationPath(declaration);
        const key = declarationKey(declaration);
        const range = compilerRange(file, declaration);
        if (!file || !key || !validCompilerRange(range)) {
          markIncomplete(coreBindingFacts, `Pyright binding ${fallbackName || declarationName(declaration)} has a declaration without a stable source anchor`);
          return '';
        }
        const name = declarationName(declaration);
        if (!name) {
          markIncomplete(coreBindingFacts, `Pyright binding ${fallbackName || '<unnamed>'} has a declaration without a stable name`);
          return '';
        }
        anchors.push({
          declaration,
          file,
          key,
          range,
          kind: declarationKind(declaration),
          name,
          canonical: `${file.buildContext}\0${key}\0${declarationKind(declaration)}\0${name}`,
        });
      }
      const buildContexts = new Set(anchors.map((anchor) => anchor.file.buildContext));
      if (buildContexts.size !== 1) {
        markIncomplete(coreBindingFacts, `Pyright binding ${fallbackName || declarationName(declarations[0])} spans multiple build contexts`);
        return '';
      }
      anchors.sort((left, right) => left.canonical < right.canonical ? -1 : left.canonical > right.canonical ? 1 : 0);
      const primary = anchors[0];
      let id;
      if (anchors.length === 1) {
        id = `pyright/${VERSION}/${primary.file.buildContext}/${encodeURIComponent(primary.file.uri)}#${primary.range.start.line}:${primary.range.start.character}:${primary.kind}:${encodeURIComponent(primary.name)}`;
      } else {
        const canonicalSet = anchors.map((anchor) => anchor.canonical).join('\n');
        const digest = crypto.createHash('sha256').update(canonicalSet).digest('hex');
        id = `pyright/${VERSION}/${primary.file.buildContext}/${encodeURIComponent(primary.file.uri)}#${primary.range.start.line}:${primary.range.start.character}:binding:${digest}`;
      }
      for (const anchor of anchors) {
        idByDeclaration.set(anchor.key, id);
        if (compilerSymbol) bindingByDeclaration.set(anchor.key, compilerSymbol);
      }
      return id;
    };
    const bindingID = (symbol, fallbackName = '') => {
      if (!symbol || typeof symbol !== 'object') return '';
      if (idByCompilerSymbol.has(symbol)) return idByCompilerSymbol.get(symbol);
      const id = bindingIDFromDeclarations(compilerDeclarations(symbol), fallbackName, symbol);
      idByCompilerSymbol.set(symbol, id);
      return id;
    };
    const send = (value) => process.stdout.write(`${JSON.stringify(value)}\n`);
    const flush = () => {
      if (!pendingCount) return;
      send({ Type: 'batch', Batch: pending });
      pending.Symbols = [];
      pending.Occurrences = [];
      pending.Edges = [];
      pendingCount = 0;
    };
    const queue = (group, value, dedupe) => {
      const json = JSON.stringify(value);
      if (Buffer.byteLength(json, 'utf8') > 1024 * 1024) fail('Pyright emitted a semantic fact larger than 1 MiB');
      if (dedupe.has(json)) return;
      dedupe.add(json);
      exportedCount++;
      exportedBytes += Buffer.byteLength(json, 'utf8');
      if (exportedCount > MAX_FACTS || exportedBytes > MAX_FACT_BYTES) fail('Pyright project exceeds the bounded semantic-fact export budget');
      pending[group].push(value);
      pendingCount++;
      if (pendingCount >= BATCH_LIMIT) flush();
    };
    const emittedSymbols = new Set();
    const emitBinding = (symbol, fallbackName = '') => {
      const declarations = compilerDeclarations(symbol).filter((declaration) => {
        return declaration && declaration.type !== 8 && ParseNodeTypeNameMap[declaration.node?.nodeType] !== 'Module';
      });
      const id = bindingID(symbol, fallbackName);
      if (!id) return '';
      if (!emittedSymbols.has(id)) {
        emittedSymbols.add(id);
        const first = declarations.map((declaration) => ({ declaration, file: fileByDeclarationPath(declaration) }))
          .filter((entry) => entry.file)
          .sort((left, right) => declarationKey(left.declaration) < declarationKey(right.declaration) ? -1 : declarationKey(left.declaration) > declarationKey(right.declaration) ? 1 : 0)[0];
        if (!first) return '';
        const fallback = path.basename(first.file.path).replace(/\.(pyi?)$/i, '');
        const name = fallbackName || symbol.getName?.() || declarationName(first.declaration, fallback);
        queue('Symbols', { ID: id, Name: name, Kind: declarationKind(first.declaration), Signature: '' }, seenSymbols);
        for (const declaration of declarations) {
          const target = fileByDeclarationPath(declaration);
          const compilerPosition = compilerRange(target, declaration);
          const range = validCompilerRange(compilerPosition) ? {
            StartLine: compilerPosition.start.line,
            StartChar: compilerPosition.start.character,
            EndLine: compilerPosition.end.line,
            EndChar: compilerPosition.end.character,
          } : null;
          if (target && range) {
            queue('Occurrences', { SymbolID: id, URI: target.uri, Range: range, Role: 'declaration' }, seenOccurrences);
            queue('Occurrences', { SymbolID: id, URI: target.uri, Range: range, Role: 'definition' }, seenOccurrences);
          }
        }
      }
      return id;
    };
    const moduleID = (file) => `pyright/${VERSION}/${file.buildContext}/module/${encodeURIComponent(file.uri)}`;
    const moduleName = (file) => {
      const source = program.getSourceFile(file.pyrightURI);
      const name = source?.getModuleName?.();
      return typeof name === 'string' && name ? name : path.basename(file.path).replace(/\.(pyi?)$/i, '');
    };
    const emitModuleSymbol = (file) => {
      const id = moduleID(file);
      queue('Symbols', { ID: id, Name: moduleName(file), Kind: 'module_graph_node', Signature: '' }, seenSymbols);
      return id;
    };
    const moduleIDs = new Map();
    for (const file of filesByPath.values()) {
      moduleIDs.set(file.uri, moduleID(file));
      if (file.scopeID === request.scopeID && scopeFilesByPath.has(file.key)) emitModuleSymbol(file);
    }

    const scopes = [];
    const supportedScopeKinds = new Set(['Module', 'Class', 'Function']);
    for (const file of scopeFilesByPath.values()) {
      totalNodes += file.nodes.length;
      if (totalNodes > MAX_TOTAL_NODES) fail('Pyright project exceeds the 1,000,000 parse-node export limit');
      for (const node of file.nodes) {
        const kind = ParseNodeTypeNameMap[node.nodeType];
        const scope = reader.get(node)?.scope;
        if (!scope) continue;
        if (supportedScopeKinds.has(kind)) {
          scopes.push({ file, node, scope, kind });
        } else if (scope.symbolTable?.size) {
          file.scopeReason = file.scopeReason || `Pyright exposed declarations in an unmodeled ${kind || 'unknown'} scope`;
        }
      }
    }
    if (scopeFilesByPath.size && !scopes.some((item) => item.kind === 'Module')) fail('Pyright did not bind module scopes for the analyzed immutable source files');
    for (const item of scopes) {
      if (item.scope.hasPotentiallyDynamicSymbolTable) {
        item.file.dynamicReason = item.file.dynamicReason || 'Pyright marked a lexical symbol table as potentially dynamic';
      }
      for (const [name, symbol] of item.scope.symbolTable || []) {
        const declarations = symbol?.getDeclarations?.() || [];
        if (!declarations.some((declaration) => declaration && declaration.type !== 8 && ParseNodeTypeNameMap[declaration.node?.nodeType] !== 'Module')) continue;
        emitBinding(symbol, name);
      }
    }
    for (const file of scopeFilesByPath.values()) {
      if (file.parseReason) markIncomplete(dynamicFacts, file.parseReason);
      if (file.scopeReason) markIncomplete(['symbol', 'declaration', 'definition', 'reference', 'call'], file.scopeReason);
    }
    const resolveType = (node) => {
      if (!node) return null;
      try {
        const result = program._evaluator.getTypeOfExpression(node);
        return result?.type || null;
      } catch (_) {
        return null;
      }
    };
    const declarationFromType = (type) => type?.shared?.declaration || null;
    const lexicalScopeFor = (node) => {
      for (let current = node; current; current = current.parent) {
        const kind = ParseNodeTypeNameMap[current.nodeType];
        if (['Module', 'Class', 'Function'].includes(kind)) {
          const scope = reader.get(current)?.scope;
          if (scope) return scope;
        }
      }
      return null;
    };
    const compilerSymbolForName = (node, name) => {
      let scope = lexicalScopeFor(node);
      while (scope) {
        const symbol = scope.symbolTable?.get(name);
        if (symbol && compilerDeclarations(symbol).length) return symbol;
        scope = scope.parent || scope.chainedModuleLevelScopeLookup;
      }
      try {
        return program._evaluator.lookUpSymbolRecursive(node, name, false) || null;
      } catch (_) {
        return null;
      }
    };
    const compilerSymbolForDeclaration = (declaration, fallbackName = '') => {
      const key = declarationKey(declaration);
      if (!key) return null;
      const known = bindingByDeclaration.get(key);
      if (known) return known;
      const name = declarationName(declaration, fallbackName);
      const node = declaration?.node;
      const nameNode = ParseNodeTypeNameMap[node?.nodeType] === 'Name' ? node : node?.d?.name;
      const matchesDeclaration = (symbol) => compilerDeclarations(symbol).some((candidate) => declarationKey(candidate) === key);
      if (nameNode && name) {
        const resolved = compilerSymbolForName(nameNode, name);
        if (resolved && matchesDeclaration(resolved)) return resolved;
      }
      // Fall back to Pyright's owning scope table only when its compiler
      // symbol contains the exact declaration; the spelling is a lookup key,
      // never an identity or merge criterion.
      let scope = lexicalScopeFor(node);
      while (scope) {
        const symbol = scope.symbolTable?.get(name);
        if (symbol && matchesDeclaration(symbol)) return symbol;
        scope = scope.parent || scope.chainedModuleLevelScopeLookup;
      }
      return null;
    };
    const resolveBinding = (node, name) => {
      const binding = compilerSymbolForName(node, name);
      if (binding && compilerDeclarations(binding).some((declaration) => declaration && declaration.type !== 8 && fileByDeclarationPath(declaration))) {
        return binding;
      }
      const declaration = declarationFromType(resolveType(node));
      return declaration && compilerSymbolForDeclaration(declaration, name);
    };
    const memberNameParent = (node) => {
      const parent = node?.parent;
      return ParseNodeTypeNameMap[parent?.nodeType] === 'MemberAccess' && parent.d?.member === node ? parent : null;
    };
    const resolveMemberReferenceID = (node, name) => {
      const memberAccess = memberNameParent(node);
      if (!memberAccess) return '';
      try {
        const declarations = program._evaluator.getDeclInfoForNameNode(node)?.decls || [];
        if (!declarations.some((declaration) => fileByDeclarationPath(declaration))) return '';
        return bindingIDFromDeclarations(declarations, name);
      } catch (_) {
        return '';
      }
    };
    const rangeForNode = (file, node) => rangeAt(file, node.start, node.start + node.length);
    const isNameDeclaration = (file, node) => {
      const parent = node.parent;
      if (!parent) return false;
      if (parent.d?.name === node) return true;
      const declaration = getDeclaration(node, reader);
      if (!declaration) return false;
      const range = compilerRange(file, declaration);
      if (!range) return false;
      const nodeRange = rangeForNode(file, node);
      return !!nodeRange && nodeRange.StartLine === range.start.line && nodeRange.StartChar === range.start.character &&
        nodeRange.EndLine === range.end.line && nodeRange.EndChar === range.end.character;
    };
    const owningNode = (node) => {
      for (let current = node?.parent; current; current = current.parent) {
        const kind = ParseNodeTypeNameMap[current.nodeType];
        if (kind === 'Function' || kind === 'Class') return current;
      }
      return null;
    };
    const ownerID = (node, file) => {
      const owner = owningNode(node);
      if (!owner) return moduleIDs.get(file.uri) || '';
      const nameNode = owner.d?.name;
      const declaration = nameNode && declarationFromType(resolveType(nameNode));
      const name = nameNode?.d?.value || '';
      return (declaration && targetID(declaration, name)) || '';
    };
    const targetID = (declaration, name) => {
      const file = fileByDeclarationPath(declaration);
      if (!file) return '';
      const key = declarationKey(declaration);
      const existing = idByDeclaration.get(key);
      if (existing) return existing;
      const binding = compilerSymbolForDeclaration(declaration, name);
      if (binding) return bindingID(binding, name);
      const node = declaration?.node;
      const nameNode = ParseNodeTypeNameMap[node?.nodeType] === 'Name' ? node : node?.d?.name;
      try {
        const declarations = program._evaluator.getDeclInfoForNameNode(nameNode)?.decls || [];
        if (declarations.some((candidate) => declarationKey(candidate) === key)) {
          return bindingIDFromDeclarations(declarations, name);
        }
      } catch (_) {
        // A declaration outside this Program's analyzed closure stays unknown.
      }
      return '';
    };
    const addEdge = (file, from, to, kind, range) => {
      if (!from || !to || !range) return;
      queue('Edges', { From: from, To: to, Kind: kind, SourceURI: file.uri, Range: range }, seenEdges);
    };
    const inTypeContext = (node) => {
      for (let current = node.parent; current; current = current.parent) {
        const kind = ParseNodeTypeNameMap[current.nodeType];
        if (kind === 'TypeAnnotation' || kind === 'TypeAlias') return true;
        if (kind === 'Function' || kind === 'Class' || kind === 'Module') return false;
      }
      return false;
    };
    const classBaseOwner = (node) => {
      for (let current = node.parent; current; current = current.parent) {
        if (ParseNodeTypeNameMap[current.nodeType] === 'Class') {
          const args = current.d?.arguments || current.d?.args || [];
          if (Array.isArray(args) && args.some((arg) => arg === node || isDescendant(arg, node))) return current;
          return null;
        }
        if (['Function', 'Module'].includes(ParseNodeTypeNameMap[current.nodeType])) return null;
      }
      return null;
    };
    const insideImport = (node) => {
      for (let current = node.parent; current; current = current.parent) {
        const kind = ParseNodeTypeNameMap[current.nodeType];
        if (kind === 'Import' || kind === 'ImportFrom') return true;
        if (['Module', 'Class', 'Function'].includes(kind)) return false;
      }
      return false;
    };
    const isDescendant = (ancestor, candidate) => {
      for (let current = candidate; current; current = current.parent) if (current === ancestor) return true;
      return false;
    };

    for (const file of scopeFilesByPath.values()) {
      for (const node of file.nodes) {
        const kind = ParseNodeTypeNameMap[node.nodeType] || '';
        if (kind === 'Name') {
          const name = node.d?.value;
          if (!name || isNameDeclaration(file, node)) continue;
          const memberAccess = memberNameParent(node);
          const resolved = memberAccess
            ? resolveMemberReferenceID(node, name)
            : bindingID(resolveBinding(node, name), name);
          if (resolved) {
            const range = rangeForNode(file, node);
            if (range) queue('Occurrences', { SymbolID: resolved, URI: file.uri, Range: range, Role: 'reference' }, seenOccurrences);
            const baseOwner = classBaseOwner(node);
            if (baseOwner) {
              const className = baseOwner.d?.name?.d?.value || '';
              const classDecl = className && declarationFromType(resolveType(baseOwner.d.name));
              const from = classDecl && targetID(classDecl, className);
              if (from) {
                addEdge(file, from, resolved, 'implementation', range);
                addEdge(file, from, resolved, 'type_relation', range);
              } else markIncomplete(['implementation', 'type_relation'], 'Pyright could not anchor a project class base to a declaration');
            } else if (inTypeContext(node)) {
              const from = ownerID(node, file);
              if (from) addEdge(file, from, resolved, 'type_relation', range);
            }
          } else if ((memberAccess || !BUILTIN_NAMES.has(name)) && !insideImport(node)) {
            markIncomplete(['reference'], `Pyright could not resolve the project ${memberAccess ? 'member ' : ''}reference ${name}`);
            if (inTypeContext(node)) markIncomplete(['type_relation'], `Pyright could not resolve the annotated type ${name}`);
          }
        }
        if (kind === 'Call') {
          const callee = node.d?.leftExpr;
          const calleeIsMember = ParseNodeTypeNameMap[callee?.nodeType] === 'MemberAccess';
          const calleeName = ParseNodeTypeNameMap[callee?.nodeType] === 'Name'
            ? callee.d?.value
            : calleeIsMember ? callee.d?.member?.d?.value : '';
          if (calleeName && DYNAMIC_CALLS.has(calleeName)) {
            file.dynamicReason = file.dynamicReason || `dynamic Python operation ${calleeName} can add facts outside the static program`;
          }
          const target = declarationFromType(resolveType(callee));
          const targetSymbol = targetID(target, declarationName(target, calleeName));
          if (targetSymbol) {
            const from = ownerID(node, file);
            const range = rangeForNode(file, node);
            if (from) addEdge(file, from, targetSymbol, 'call', range);
            else markIncomplete(['call'], 'Pyright could not anchor the enclosing call owner');
          } else if (calleeIsMember || (calleeName && !BUILTIN_NAMES.has(calleeName))) {
            markIncomplete(['call'], `Pyright could not resolve the call target ${calleeName || '<member>'}`);
          }
        }
        if (kind === 'Class') {
          const decorators = node.d?.decorators;
          if (Array.isArray(decorators) && decorators.length) file.dynamicReason = file.dynamicReason || 'decorated classes can be replaced or extended at runtime';
          const args = node.d?.args || [];
          if (Array.isArray(args) && args.some((arg) => arg.d?.name?.d?.value === 'metaclass')) {
            file.dynamicReason = file.dynamicReason || 'a custom metaclass can change the runtime class graph';
          }
        }
        if (kind === 'Function') {
          const name = node.d?.name?.d?.value;
          if (['__getattr__', '__getattribute__', '__setattr__', '__dir__'].includes(name)) {
            file.dynamicReason = file.dynamicReason || `${name} can synthesize Python attributes at runtime`;
          }
          const decorators = node.d?.decorators;
          if (Array.isArray(decorators) && decorators.length) file.dynamicReason = file.dynamicReason || 'decorated functions can be replaced at runtime';
        }
        if (kind === 'ImportFrom' && (node.d?.isWildcardImport || node.d?.wildcardImport)) {
          file.importIncomplete = file.importIncomplete || 'wildcard imports prevent exact imported-symbol enumeration';
        }
      }
    }

    for (const file of scopeFilesByPath.values()) {
      const sourceModule = moduleIDs.get(file.uri);
      const namesByImport = new Map();
      for (const candidate of file.nodes) {
        if (ParseNodeTypeNameMap[candidate.nodeType] !== 'Name') continue;
        for (let parent = candidate.parent; parent; parent = parent.parent) {
          const parentKind = ParseNodeTypeNameMap[parent.nodeType];
          if (parentKind === 'Import' || parentKind === 'ImportFrom') {
            if (!namesByImport.has(parent)) namesByImport.set(parent, []);
            namesByImport.get(parent).push(candidate);
            break;
          }
          if (['Module', 'Class', 'Function'].includes(parentKind)) break;
        }
      }
      for (const node of file.nodes) {
        const kind = ParseNodeTypeNameMap[node.nodeType];
        if (kind !== 'Import' && kind !== 'ImportFrom') continue;
        const importNames = [];
        for (const candidate of namesByImport.get(node) || []) {
          const declaration = declarationFromType(resolveType(candidate));
          if (!declaration) continue;
          const symbol = targetID(declaration, candidate.d?.value || '');
          if (!symbol) {
            const importedPath = declaration.uri?.getFilePath?.();
            const importedFile = importedPath && filesByPath.get(pathKey(importedPath));
            if (!importedFile) continue;
          }
          const targetFile = fileByDeclarationPath(declaration);
          if (!targetFile) continue;
          const targetModule = moduleIDs.get(targetFile.uri);
          const range = rangeForNode(file, candidate);
          if (!range) continue;
          if (symbol) queue('Occurrences', { SymbolID: symbol, URI: file.uri, Range: range, Role: 'reference' }, seenOccurrences);
          importNames.push({ symbol, targetModule, range });
        }
        if (!importNames.length) {
          file.importIncomplete = file.importIncomplete || 'Pyright did not resolve an import to a source file in the immutable scope';
          continue;
        }
        const statementRange = rangeForNode(file, node);
        const uniqueModules = new Set();
        for (const imported of importNames) {
          if (imported.symbol) addEdge(file, sourceModule, imported.symbol, 'import', imported.range);
          if (imported.targetModule) uniqueModules.add(imported.targetModule);
        }
        for (const targetModule of uniqueModules) addEdge(file, sourceModule, targetModule, 'module', statementRange);
      }
    }

    const coverage = [];
    for (const fact of FACTS) {
      let State = 'complete';
      let Reason = '';
      if (fact === 'include') {
        State = 'unavailable';
        Reason = 'preprocessor include relationships do not apply to Python';
      } else if (fact === 'implementation') {
        State = 'incomplete_known_subset';
        Reason = 'the exporter records class-base relations; protocol and method implementation relations are not exhaustive';
      } else if (fact === 'type_relation') {
        State = 'incomplete_known_subset';
        Reason = 'the exporter records explicit bases and simple annotations; inferred assignment and union relations are not exhaustive';
      } else if (fact === 'import') {
        State = 'incomplete_known_subset';
        Reason = 'the exporter records explicit project-symbol imports; re-exports and implicit package imports are not exhaustive';
      } else if (fact === 'module') {
        State = 'incomplete_known_subset';
        Reason = 'the exporter records source-level project imports; namespace packages and implicit module dependencies are not exhaustive';
      } else if (fact === 'generated_source' && Array.from(scopeFilesByPath.values()).some((file) => file.generated)) {
        State = 'incomplete_known_subset';
        Reason = 'generated Python sources do not have an exhaustive compiler range-map attestation';
      } else if (fact === 'import' || fact === 'module') {
        const reason = Array.from(scopeFilesByPath.values()).find((file) => file.importIncomplete)?.importIncomplete;
        if (reason) {
          State = 'incomplete_known_subset';
          Reason = reason;
        }
      }
      if (factIncomplete[fact]) {
        State = 'incomplete_known_subset';
        Reason = factIncomplete[fact];
      }
      const dynamic = Array.from(scopeFilesByPath.values()).find((file) => file.dynamicReason)?.dynamicReason;
      if (dynamic && dynamicFacts.includes(fact)) {
        State = 'incomplete_known_subset';
        Reason = dynamic;
      }
      coverage.push({ ScopeID: request.scopeID, Fact: fact, State, Reason });
    }
    flush();
    return { Coverage: coverage };
  } finally {
    service.dispose();
  }
}

async function main() {
  let raw = '';
  let rawBytes = 0;
  for await (const chunk of process.stdin) {
    rawBytes += Buffer.isBuffer(chunk) ? chunk.length : Buffer.byteLength(String(chunk), 'utf8');
    if (rawBytes > 192 * 1024 * 1024) fail('Pyright exporter request exceeds the 192 MiB input limit');
    raw += chunk;
  }
  try {
    const request = JSON.parse(raw);
    const result = run(request);
    process.stdout.write(`${JSON.stringify({ Type: 'result', Result: result })}\n`);
  } catch (error) {
    process.stderr.write(`PYRIGHT_EXPORT_FAILURE: ${error && error.stack ? error.stack : String(error)}\n`);
    process.stdout.write(`${JSON.stringify({ Type: 'error', Error: error?.message || String(error) })}\n`);
    process.exitCode = 1;
  }
}

main();
