'use strict';

// This is intentionally a small, one-shot TypeScript compiler adapter. It
// receives a materialized immutable project over stdin and emits JSON objects
// over stdout. The Go side owns snapshot identity, range normalization, and
// bounded sink writes; this side owns only Program/TypeChecker facts.

const crypto = require('crypto');
const fs = require('fs');
const path = require('path');

const FACTS = [
  'symbol', 'declaration', 'definition', 'reference', 'implementation',
  'type_relation', 'call', 'import', 'include', 'module', 'generated_source',
];
const EXPORT_BATCH_LIMIT = 256;
const MAX_REQUEST_BYTES = 128 * 1024 * 1024;
const MAX_PROJECT_SOURCE_BYTES = 64 * 1024 * 1024;
const MAX_EXPORT_FACT_BYTES = 64 * 1024 * 1024;
const MAX_EXPORT_SINGLE_FACT_BYTES = 1024 * 1024;
const MAX_EXPORT_FACTS = 1000000;
const MAX_EXPORT_SYMBOLS = 250000;

function send(value) {
  const output = Buffer.from(JSON.stringify(value) + '\n', 'utf8');
  let offset = 0;
  while (offset < output.length) offset += fs.writeSync(1, output, offset, output.length - offset);
}

function unknownCoverage(scopeID, reason) {
  return FACTS.map((fact) => ({
    ScopeID: scopeID,
    Fact: fact,
    State: fact === 'include' ? 'unavailable' : 'unknown',
    Reason: fact === 'include' ? 'preprocessor include relationships do not apply to TypeScript' : reason,
  }));
}

function canonicalPath(value) {
  return path.normalize(path.resolve(value));
}

function pathKey(value) {
  const normalized = canonicalPath(value);
  return process.platform === 'win32' ? normalized.toLowerCase() : normalized;
}

function inside(root, value) {
  const rootPath = canonicalPath(root);
  const candidate = canonicalPath(value);
  const relative = path.relative(rootPath, candidate);
  return relative === '' || (relative !== '..' && !relative.startsWith('..' + path.sep) && !path.isAbsolute(relative));
}

function safelyInside(root, value) {
  const candidate = canonicalPath(value);
  if (!inside(root, candidate)) return false;
  try {
    return inside(root, fs.realpathSync.native(candidate));
  } catch (_) {
    // A missing path cannot be used to read outside the root.
    return true;
  }
}

function stableJSON(value) {
  if (Array.isArray(value)) return '[' + value.map(stableJSON).join(',') + ']';
  if (value && typeof value === 'object') {
    return '{' + Object.keys(value).sort().map((key) => JSON.stringify(key) + ':' + stableJSON(value[key])).join(',') + '}';
  }
  return JSON.stringify(value);
}

function sameJSON(left, right) {
  try {
    return stableJSON(left) === stableJSON(right);
  } catch (_) {
    return false;
  }
}

function optionValue(options, names) {
  if (!options) return '';
  for (const name of names) {
    for (const key of Object.keys(options)) {
      if (key.toLowerCase().replace(/[_. -]/g, '') === name) return options[key];
    }
  }
  return '';
}

function sourcePosition(sourceFile, position) {
  const start = sourceFile.getLineAndCharacterOfPosition(position.start);
  const end = sourceFile.getLineAndCharacterOfPosition(position.end);
  return {
    StartLine: start.line,
    StartChar: start.character,
    EndLine: end.line,
    EndChar: end.character,
  };
}

function syntaxKindName(ts, node) {
  if (!node) return 'symbol';
  if (ts.isClassLike(node)) return 'class';
  if (ts.isInterfaceDeclaration(node)) return 'interface';
  if (ts.isEnumDeclaration(node)) return 'enum';
  if (ts.isTypeAliasDeclaration(node)) return 'type';
  if (ts.isFunctionDeclaration(node) || ts.isMethodDeclaration(node) || ts.isMethodSignature(node)) return 'method';
  if (ts.isConstructorDeclaration(node)) return 'constructor';
  if (ts.isGetAccessorDeclaration(node) || ts.isSetAccessorDeclaration(node)) return 'property';
  if (ts.isPropertyDeclaration(node) || ts.isPropertySignature(node) || ts.isPropertyAssignment(node)) return 'property';
  if (ts.isVariableDeclaration(node) || ts.isBindingElement(node)) return 'variable';
  if (ts.isModuleDeclaration(node)) return 'module';
  if (ts.isTypeParameterDeclaration(node)) return 'type_parameter';
  if (ts.isParameter(node)) return 'parameter';
  return 'symbol';
}

function createParseHost(ts, rootPath, compilerRoot, readGuarded, fileByPath, markOutOfScope) {
  return {
    useCaseSensitiveFileNames: ts.sys.useCaseSensitiveFileNames,
    readFile: readGuarded,
    fileExists: (value) => readGuarded(value) !== undefined,
    directoryExists: (value) => safelyInside(rootPath, value) && fs.existsSync(value) && fs.statSync(value).isDirectory(),
    getDirectories: (value) => {
      if (!safelyInside(rootPath, value)) return [];
      try {
        return fs.readdirSync(value, { withFileTypes: true }).filter((entry) => entry.isDirectory()).map((entry) => path.join(value, entry.name));
      } catch (_) {
        return [];
      }
    },
    readDirectory: (value, extensions, excludes, includes, depth) => {
      if (!safelyInside(rootPath, value)) {
        markOutOfScope('TypeScript config requested a source directory outside the immutable scope');
        return [];
      }
      let candidates;
      try {
        // Use TypeScript's own glob engine so include, exclude, extension and
        // depth semantics match parseJsonConfigFileContent.
        candidates = ts.sys.readDirectory(value, extensions, excludes, includes, depth);
      } catch (_) {
        markOutOfScope('TypeScript project file patterns could not be evaluated');
        return [];
      }
      return candidates.filter((candidate) => {
        const resolved = canonicalPath(candidate);
        if (!safelyInside(rootPath, resolved)) {
          markOutOfScope('TypeScript project file resolves outside the immutable scope');
          return false;
        }
        if (!fileByPath.has(pathKey(resolved))) {
          markOutOfScope('TypeScript project file is absent from the immutable scope manifest');
          return false;
        }
        return true;
      });
    },
    realpath: (value) => {
      const resolved = canonicalPath(value);
      try {
        const real = fs.realpathSync.native(resolved);
        if (inside(rootPath, real) || inside(compilerRoot, real)) return real;
      } catch (_) {
        // Let the caller report a missing path through fileExists/readFile.
      }
      return resolved;
    },
    onUnRecoverableConfigFileDiagnostic: () => {},
  };
}

function run(request) {
  const compilerPath = canonicalPath(request.compilerPath);
  const rootPath = canonicalPath(request.rootPath);
  const compilerRoot = canonicalPath(path.dirname(compilerPath));
  const scopeID = request.scopeID || '';
  const buildContext = typeof request.buildContext === 'string' ? request.buildContext.trim() : '';
  const baseReason = 'TypeScript compiler project could not be proven complete';

  if (!buildContext) {
    return { result: { Coverage: unknownCoverage(scopeID, 'TypeScript build context is unavailable for semantic identities') } };
  }

  if (!inside(rootPath, rootPath) || !fs.existsSync(rootPath) || !fs.statSync(rootPath).isDirectory()) {
    return { result: { Coverage: unknownCoverage(scopeID, 'materialized TypeScript root is unavailable') } };
  }
  if (!fs.existsSync(compilerPath)) {
    return { result: { Coverage: unknownCoverage(scopeID, 'pinned TypeScript compiler module is unavailable') } };
  }

  let ts;
  try {
    ts = require(compilerPath);
  } catch (error) {
    return { result: { Coverage: unknownCoverage(scopeID, 'pinned TypeScript compiler could not be loaded: ' + error.message) } };
  }
  if (ts.version !== '6.0.3') {
    return { result: { Coverage: unknownCoverage(scopeID, 'loaded TypeScript version is ' + String(ts.version)) } };
  }

  const configPath = canonicalPath(request.configPath);
  const scopeRootPath = canonicalPath(request.scopeRootPath || path.dirname(configPath));
  if (!safelyInside(rootPath, scopeRootPath) || !inside(scopeRootPath, configPath)) {
    return { result: { Coverage: unknownCoverage(scopeID, 'tsconfig/jsconfig resolves outside the immutable project root') } };
  }

  const inputs = [{
    scopeID,
    rootPath: scopeRootPath,
    buildContext,
    configPath,
    expectedConfigDigest: request.expectedConfigDigest || '',
    expectedCompilerOptions: request.expectedCompilerOptions || '',
    expectedProjectReferences: request.expectedProjectReferences || '',
    files: Array.isArray(request.files) ? request.files : [],
  }];
  for (const reference of (Array.isArray(request.referenceScopes) ? request.referenceScopes : [])) {
    if (!reference || typeof reference.scopeID !== 'string' || !reference.scopeID || typeof reference.buildContext !== 'string' || !reference.buildContext) {
      return { result: { Coverage: unknownCoverage(scopeID, 'referenced TypeScript scope identity is incomplete') } };
    }
    inputs.push({
      scopeID: reference.scopeID,
      rootPath: canonicalPath(reference.rootPath),
      buildContext: reference.buildContext,
      configPath: canonicalPath(reference.configPath),
      expectedConfigDigest: reference.expectedConfigDigest || '',
      expectedCompilerOptions: reference.expectedCompilerOptions || '',
      expectedProjectReferences: reference.expectedProjectReferences || '',
      files: Array.isArray(reference.files) ? reference.files : [],
    });
  }

  const inputByID = new Map();
  const inputByConfig = new Map();
  const fileByPath = new Map();
  const manifestByScope = new Map();
  let outsideProjectFiles = false;
  let outsideProjectReason = '';
  const markOutOfScope = (reason) => {
    outsideProjectFiles = true;
    if (!outsideProjectReason) outsideProjectReason = reason;
  };
  for (const input of inputs) {
    if (!input.scopeID || inputByID.has(input.scopeID) || !input.buildContext ||
        !safelyInside(rootPath, input.rootPath) || !inside(input.rootPath, input.configPath) || !safelyInside(rootPath, input.configPath)) {
      return { result: { Coverage: unknownCoverage(scopeID, 'TypeScript project-reference scope path or identity is invalid') } };
    }
    const configKey = pathKey(input.configPath);
    if (inputByConfig.has(configKey)) {
      return { result: { Coverage: unknownCoverage(scopeID, 'TypeScript project-reference closure repeats a config path') } };
    }
    inputByID.set(input.scopeID, input);
    inputByConfig.set(configKey, input);
    const manifest = new Set();
    for (const file of input.files) {
      if (!file || typeof file.path !== 'string' || !file.path || typeof file.uri !== 'string' || !file.uri) {
        return { result: { Coverage: unknownCoverage(scopeID, 'TypeScript source manifest contains an invalid entry') } };
      }
      const resolved = canonicalPath(file.path);
      const key = pathKey(resolved);
      if (!safelyInside(rootPath, resolved) || !inside(input.rootPath, resolved)) {
        return { result: { Coverage: unknownCoverage(scopeID, 'TypeScript source manifest escapes its captured project root') } };
      }
      const prior = fileByPath.get(key);
      if (prior && prior.uri !== file.uri) {
        return { result: { Coverage: unknownCoverage(scopeID, 'TypeScript source manifest maps one path to conflicting URIs') } };
      }
      fileByPath.set(key, file);
      manifest.add(key);
    }
    manifestByScope.set(input.scopeID, manifest);
    if (!manifest.has(configKey) || !fs.existsSync(input.configPath)) {
      return { result: { Coverage: unknownCoverage(scopeID, 'TypeScript project config is absent from its captured file manifest') } };
    }
  }

  const readGuarded = (value) => {
    const candidate = canonicalPath(value);
    if (!safelyInside(rootPath, candidate) && !safelyInside(compilerRoot, candidate)) return undefined;
    try {
      return fs.readFileSync(candidate, 'utf8');
    } catch (_) {
      return undefined;
    }
  };
  const parseHost = createParseHost(ts, rootPath, compilerRoot, readGuarded, fileByPath, markOutOfScope);
  const parsedScopes = new Map();
  for (const input of inputs) {
    let expectedOptions;
    try {
      expectedOptions = JSON.parse(input.expectedCompilerOptions);
    } catch (_) {
      return { result: { Coverage: unknownCoverage(scopeID, 'scoped compiler options are not valid JSON') } };
    }
    let optionLayers = [];
    if (expectedOptions && expectedOptions.schema === 'omnilsp-tsconfig-v1') {
      if (!expectedOptions.complete) {
        return { result: { Coverage: unknownCoverage(scopeID, expectedOptions.reason || 'TypeScript extends configuration closure is incomplete') } };
      }
      if (!Array.isArray(expectedOptions.configs) || !expectedOptions.configs.length) {
        return { result: { Coverage: unknownCoverage(scopeID, 'scoped TypeScript config closure is empty') } };
      }
      const seenConfigs = new Set();
      for (const item of expectedOptions.configs) {
        if (!item || typeof item.path !== 'string' || typeof item.content !== 'string' || !item.compilerOptions || typeof item.compilerOptions !== 'object') {
          return { result: { Coverage: unknownCoverage(scopeID, 'scoped TypeScript config closure has an invalid entry') } };
        }
        const configFilePath = canonicalPath(path.resolve(input.rootPath, item.path));
        const key = pathKey(configFilePath);
        if (seenConfigs.has(key) || !safelyInside(rootPath, configFilePath)) {
          return { result: { Coverage: unknownCoverage(scopeID, 'scoped TypeScript config closure repeats or escapes the captured workspace') } };
        }
        seenConfigs.add(key);
        try {
          if (fs.readFileSync(configFilePath, 'utf8') !== item.content) {
            return { result: { Coverage: unknownCoverage(scopeID, 'scoped TypeScript config closure differs from the materialized snapshot') } };
          }
        } catch (_) {
          return { result: { Coverage: unknownCoverage(scopeID, 'scoped TypeScript config closure is absent from the materialized snapshot') } };
        }
        optionLayers.push({ path: configFilePath, options: item.compilerOptions });
      }
      if (!seenConfigs.has(pathKey(input.configPath))) {
        return { result: { Coverage: unknownCoverage(scopeID, 'scoped TypeScript config closure omits the project config') } };
      }
    } else if (expectedOptions && typeof expectedOptions === 'object' && !Array.isArray(expectedOptions)) {
      optionLayers = [{ path: input.configPath, options: expectedOptions }];
    } else {
      return { result: { Coverage: unknownCoverage(scopeID, 'scoped compiler options are not an object') } };
    }

    let actualDigest = '';
    if (!input.expectedConfigDigest) {
      return { result: { Coverage: unknownCoverage(scopeID, 'project config digest is absent from the scoped build identity') } };
    }
    try {
      actualDigest = 'sha256:' + crypto.createHash('sha256').update(fs.readFileSync(input.configPath)).digest('hex');
    } catch (_) {
      return { result: { Coverage: unknownCoverage(scopeID, 'tsconfig/jsconfig digest could not be computed') } };
    }
    const expectedDigest = String(input.expectedConfigDigest).toLowerCase();
    if (expectedDigest && actualDigest !== expectedDigest && actualDigest.slice('sha256:'.length) !== expectedDigest.replace(/^sha256:/, '')) {
      return { result: { Coverage: unknownCoverage(scopeID, 'tsconfig/jsconfig digest does not match the scoped build identity') } };
    }
    if (!input.expectedProjectReferences) {
      return { result: { Coverage: unknownCoverage(scopeID, 'project references are absent from the scoped build identity') } };
    }
    let expectedReferences;
    try {
      expectedReferences = JSON.parse(input.expectedProjectReferences);
    } catch (_) {
      return { result: { Coverage: unknownCoverage(scopeID, 'project references are invalid JSON in the scoped build identity') } };
    }

    const configFile = ts.readConfigFile(input.configPath, readGuarded);
    if (configFile.error || !configFile.config) {
      return { result: { Coverage: unknownCoverage(scopeID, 'tsconfig/jsconfig could not be read') } };
    }
    const configSourceFile = ts.readJsonConfigFile(input.configPath, readGuarded);
    if (!configSourceFile || (configSourceFile.parseDiagnostics && configSourceFile.parseDiagnostics.length)) {
      return { result: { Coverage: unknownCoverage(scopeID, 'tsconfig/jsconfig could not be parsed as a source config') } };
    }
    const actualReferences = Array.isArray(configFile.config.references) ? configFile.config.references : [];
    if (!sameJSON(expectedReferences, actualReferences)) {
      return { result: { Coverage: unknownCoverage(scopeID, 'project references differ from the scoped build identity') } };
    }
    const parsed = ts.parseJsonSourceFileConfigFileContent(configSourceFile, parseHost, path.dirname(input.configPath), undefined, input.configPath);
    if (parsed.errors && parsed.errors.length) {
      return { result: { Coverage: unknownCoverage(scopeID, 'tsconfig/jsconfig has unrecoverable compiler configuration diagnostics') } };
    }
    try {
      const expected = {};
      for (const layer of optionLayers) {
        const converted = ts.convertCompilerOptionsFromJson(layer.options, path.dirname(layer.path), layer.path);
        if (converted.errors && converted.errors.length) {
          return { result: { Coverage: unknownCoverage(scopeID, 'scoped compiler options contain unsupported or invalid values') } };
        }
        Object.assign(expected, converted.options || {});
      }
      if (Object.keys(expected).some((key) => !sameJSON(expected[key], parsed.options[key]))) {
        return { result: { Coverage: unknownCoverage(scopeID, 'effective compiler options differ from the scoped build identity') } };
      }
    } catch (_) {
      return { result: { Coverage: unknownCoverage(scopeID, 'scoped compiler options could not be normalized') } };
    }
    const projectFiles = new Set();
    for (const fileName of parsed.fileNames || []) {
      const resolved = canonicalPath(fileName);
      const key = pathKey(resolved);
      if (!inside(input.rootPath, resolved) || !manifestByScope.get(input.scopeID).has(key)) {
        return { result: { Coverage: unknownCoverage(scopeID, 'TypeScript project file is outside or absent from its immutable scope manifest') } };
      }
      projectFiles.add(key);
    }
    if (input.scopeID !== scopeID && !parsed.options.composite) {
      return { result: { Coverage: unknownCoverage(scopeID, 'referenced TypeScript project is not composite') } };
    }
    parsedScopes.set(input.scopeID, { input, configFile, parsed, projectFiles });
  }

  const current = parsedScopes.get(scopeID);
  if (!current) return { result: { Coverage: unknownCoverage(scopeID, 'current TypeScript project was not parsed') } };
  const referenceScopeByConfig = new Map();
  for (const input of inputs.slice(1)) referenceScopeByConfig.set(pathKey(input.configPath), parsedScopes.get(input.scopeID));
  const reachableReferences = new Set();
  const activeReferences = new Set();
  const visitReference = (reference) => {
    let referencePath;
    try {
      referencePath = canonicalPath(ts.resolveProjectReferencePath(reference));
    } catch (_) {
      return 'TypeScript project reference path could not be resolved';
    }
    const key = pathKey(referencePath);
    if (!safelyInside(rootPath, referencePath)) return 'TypeScript project reference resolves outside the immutable workspace boundary';
    if (activeReferences.has(key)) return 'TypeScript project-reference closure contains a cycle';
    const target = referenceScopeByConfig.get(key);
    if (!target) return 'TypeScript project-reference closure omits config ' + referencePath;
    if (reachableReferences.has(key)) return '';
    activeReferences.add(key);
    for (const child of target.parsed.projectReferences || []) {
      const error = visitReference(child);
      if (error) return error;
    }
    activeReferences.delete(key);
    reachableReferences.add(key);
    return '';
  };
  for (const reference of current.parsed.projectReferences || []) {
    const error = visitReference(reference);
    if (error) return { result: { Coverage: unknownCoverage(scopeID, error) } };
  }
  if (reachableReferences.size !== inputs.length - 1) {
    return { result: { Coverage: unknownCoverage(scopeID, 'TypeScript project-reference closure contains unreferenced project scopes') } };
  }
  const parsed = current.parsed;
  const projectFiles = current.projectFiles;
  const javascriptExtensions = new Set(['.js', '.mjs', '.cjs']);
  const isJavaScriptPath = (value) => javascriptExtensions.has(path.extname(value).toLowerCase());
  const isJavaScriptManifestEntry = (file) => {
    if (!file || typeof file !== 'object') return false;
    return isJavaScriptPath(String(file.path || '')) ||
      /javascript/i.test(String(file.language || '')) ||
      /\.(?:jsx|tsx)$/i.test(String(file.path || ''));
  };
  const jsQualificationRequested = current.input.files.some(isJavaScriptManifestEntry);
  let jsQualificationFailure = '';
  const rejectJSQualification = (reason) => {
    if (jsQualificationRequested && !jsQualificationFailure) jsQualificationFailure = reason;
  };
  if (jsQualificationRequested && inputs.length !== 1) {
    rejectJSQualification('JavaScript project references are outside the current single-project checkJs proof');
  }
  const typescriptSourceExtensions = new Set(['.ts', '.mts', '.cts']);
  const staticTypeScriptModuleCandidate = !jsQualificationRequested && inputs.length === 1 && parsedScopes.size === 1 &&
    !(parsed.projectReferences || []).length && projectFiles.size > 0 &&
    Array.from(projectFiles).every((key) => typescriptSourceExtensions.has(path.extname(key).toLowerCase()));
  let staticTypeScriptModuleClosureComplete = staticTypeScriptModuleCandidate;

  const contextByFile = new Map();
  const ambiguousDeclarationFiles = new Set();
  for (const value of parsedScopes.values()) {
    for (const key of value.projectFiles) {
      const prior = contextByFile.get(key);
      if (prior && prior !== value.input.buildContext) ambiguousDeclarationFiles.add(key);
      else contextByFile.set(key, value.input.buildContext);
    }
  }

  const originalHost = ts.createCompilerHost(parsed.options, true);
  originalHost.getCurrentDirectory = () => rootPath;
  originalHost.getParsedCommandLine = (referenceConfigPath) => {
    const referenced = parsedScopes.get((inputByConfig.get(pathKey(referenceConfigPath)) || {}).scopeID);
    if (!referenced || referenced.input.scopeID === scopeID) {
      markOutOfScope('TypeScript compiler requested a project config outside the verified reference closure');
      return undefined;
    }
    return referenced.parsed;
  };
  originalHost.useSourceOfProjectReferenceRedirect = () => true;
  const originalRead = originalHost.readFile.bind(originalHost);
  const originalExists = originalHost.fileExists.bind(originalHost);
  const guardedPath = (value) => {
    const candidate = canonicalPath(value);
    return safelyInside(rootPath, candidate) || safelyInside(compilerRoot, candidate);
  };
  originalHost.readFile = (value) => guardedPath(value) ? originalRead(value) : undefined;
  originalHost.fileExists = (value) => guardedPath(value) && originalExists(value);
  const originalDirectory = originalHost.directoryExists ? originalHost.directoryExists.bind(originalHost) : undefined;
  if (originalDirectory) originalHost.directoryExists = (value) => guardedPath(value) && originalDirectory(value);
  const originalRealpath = originalHost.realpath ? originalHost.realpath.bind(originalHost) : undefined;
  if (originalRealpath) {
    originalHost.realpath = (value) => {
      if (!guardedPath(value)) return canonicalPath(value);
      return originalRealpath(value);
    };
  }
  originalHost.writeFile = () => {};
  const originalGetSourceFile = originalHost.getSourceFile.bind(originalHost);
  const programFilesSeen = new Set();
  let programSourceBytes = 0;
  originalHost.getSourceFile = (fileName, languageVersion, onError, shouldCreateNewSourceFile) => {
    if (!guardedPath(fileName)) {
      // TypeScript probes conventional @types/node_modules locations even
      // when they do not exist. Only an existing outside file is evidence of
      // an untracked project dependency.
      try {
        if (fs.existsSync(fileName)) outsideProjectFiles = true;
      } catch (_) {
        outsideProjectFiles = true;
      }
      return undefined;
    }
    const resolved = canonicalPath(fileName);
    const key = pathKey(resolved);
    if (inside(rootPath, resolved) && !programFilesSeen.has(key)) {
      let sourceBytes = 0;
      try {
        sourceBytes = fs.statSync(resolved).size;
      } catch (_) {
        outsideProjectFiles = true;
        if (!outsideProjectReason) outsideProjectReason = 'TypeScript project source could not be statted';
        return undefined;
      }
      programFilesSeen.add(key);
      programSourceBytes += sourceBytes;
      if (programSourceBytes > MAX_PROJECT_SOURCE_BYTES) {
        throw new Error('TypeScript project exceeds the 64 MiB source-text budget; export unavailable');
      }
    }
    return originalGetSourceFile(fileName, languageVersion, onError, shouldCreateNewSourceFile);
  };
  const program = ts.createProgram({
    rootNames: Array.from(projectFiles).map((key) => fileByPath.get(key)?.path).filter(Boolean).sort(),
    options: parsed.options,
    host: originalHost,
    projectReferences: parsed.projectReferences,
  });
  let programHasErrors = false;
  const programSourceFiles = new Set(program.getSourceFiles().map((sourceFile) => pathKey(sourceFile.fileName)));
  const capturedSourceOwners = new Map();
  for (const input of inputs) {
    const manifest = manifestByScope.get(input.scopeID) || new Set();
    for (const key of manifest) {
      const owners = capturedSourceOwners.get(key) || new Set();
      owners.add(input.scopeID);
      capturedSourceOwners.set(key, owners);
    }
  }
  for (const value of parsedScopes.values()) {
    if (value.input.scopeID === scopeID) continue;
    for (const key of value.projectFiles) {
      if (!programSourceFiles.has(key)) {
        outsideProjectFiles = true;
        if (!outsideProjectReason) outsideProjectReason = "a referenced project's captured sources were not loaded by the TypeScript Program";
      }
    }
  }
  const checker = program.getTypeChecker();
  const knownSourceFiles = new Set();
  const symbolIDs = new Map();
  const pending = { Symbols: [], Occurrences: [], Edges: [] };
  let pendingFactCount = 0;
  let exportedFactCount = 0;
  let exportedFactBytes = 0;
  let exportedSymbolCount = 0;
  const flushFacts = () => {
    if (!pendingFactCount) return;
    send({ Type: 'batch', Batch: pending });
    pending.Symbols = [];
    pending.Occurrences = [];
    pending.Edges = [];
    pendingFactCount = 0;
  };
  const queueFact = (group, value) => {
    const encodedBytes = Buffer.byteLength(JSON.stringify(value), 'utf8');
    if (encodedBytes > MAX_EXPORT_SINGLE_FACT_BYTES) {
      throw new Error('TypeScript project contains a semantic fact larger than the 1 MiB fact limit; export unavailable');
    }
    exportedFactCount++;
    exportedFactBytes += encodedBytes;
    if (exportedFactCount > MAX_EXPORT_FACTS || exportedFactBytes > MAX_EXPORT_FACT_BYTES) {
      throw new Error('TypeScript project exceeds the bounded semantic-fact export budget; export unavailable');
    }
    if (group === 'Symbols' && ++exportedSymbolCount > MAX_EXPORT_SYMBOLS) {
      throw new Error('TypeScript project exceeds the 250000-symbol export budget; export unavailable');
    }
    pending[group].push(value);
    pendingFactCount++;
    if (pendingFactCount >= EXPORT_BATCH_LIMIT) flushFacts();
  };
  let externalFacts = outsideProjectFiles;
  let externalReason = outsideProjectFiles ? (outsideProjectReason || 'outside project file') : '';
  const markExternal = (reason) => {
    externalFacts = true;
    if (!externalReason) externalReason = reason;
  };
  let dynamicFacts = false;
  let generatedFacts = false;
  let unresolvedCallTargetReason = '';
  let moduleRuntimeEscapeReason = '';
  const markModuleRuntimeEscape = (reason) => {
    const isFallback = (value) => value.startsWith('any or unknown call targets') ||
      value.startsWith('a call target without a body') ||
      value.startsWith('a call target or owner could not be linked');
    // This guard qualifies only import/module closure. Do not let its
    // call-target checks demote unrelated TypeScript symbol and type facts.
    if (!moduleRuntimeEscapeReason || (isFallback(moduleRuntimeEscapeReason) && !isFallback(reason))) {
      moduleRuntimeEscapeReason = reason;
    }
  };
  const incompleteReasons = Object.create(null);
  const markIncomplete = (fact, reason) => {
    incompleteReasons[fact] = incompleteReasons[fact] || reason;
    if (['symbol', 'declaration', 'definition', 'reference'].includes(fact)) rejectJSQualification(reason);
  };
  const markReferenceIncomplete = (reason) => {
    markIncomplete('reference', reason);
    rejectJSQualification(reason);
  };
  const isDynamicRuntimeName = (name) => name === 'require' || name === 'eval';
  let typeRelationReason = '';
  const markTypeRelationIncomplete = (reason) => {
    markIncomplete('type_relation', reason);
    if (!typeRelationReason) typeRelationReason = reason;
  };

  const isCompilerSource = (sourceFile) => {
    if (!sourceFile) return false;
    return inside(compilerRoot, sourceFile.fileName);
  };
  // A Complete JavaScript query profile is considered only when every source
  // in the captured project-reference closure is checked, owned, and statically
  // representable by this visitor. TypeScript continues to use its existing
  // static-reference profile below.
  if (jsQualificationRequested) {
    for (const value of parsedScopes.values()) {
      if (value.parsed.options.allowJs !== true || value.parsed.options.checkJs !== true) {
        rejectJSQualification('every captured JavaScript project must enable allowJs and checkJs');
      }
      for (const key of value.projectFiles) {
        const file = fileByPath.get(key);
        const extension = path.extname(key).toLowerCase();
        if (!javascriptExtensions.has(extension)) {
          rejectJSQualification('the captured project-reference closure contains a non-JavaScript or JSX source');
        }
        if (file && /react/i.test(String(file.language || ''))) {
          rejectJSQualification('JSX and JavaScript React sources are outside the static checkJs profile');
        }
        if (file && file.generated) {
          rejectJSQualification('generated source has no exhaustive range-level reference proof');
        }
      }
    }
  }
  for (const input of inputs) {
    for (const file of input.files) {
      const extension = path.extname(file.path).toLowerCase();
      const language = String(file.language || '').toLowerCase();
      if (['.jsx', '.tsx'].includes(extension) || language.includes('react')) {
        markReferenceIncomplete('JSX and JavaScript React sources are outside the static checkJs profile');
      }
    }
  }
  for (const key of projectFiles) {
    if (!programSourceFiles.has(key)) {
      staticTypeScriptModuleClosureComplete = false;
      markReferenceIncomplete('a configured TypeScript source file was not loaded by the compiler Program');
    }
  }
  for (const sourceFile of program.getSourceFiles()) {
    if (isCompilerSource(sourceFile)) continue;
    const key = pathKey(sourceFile.fileName);
    const owners = capturedSourceOwners.get(key);
    if (!owners || !owners.size || !fileByPath.has(key)) {
      staticTypeScriptModuleClosureComplete = false;
      externalFacts = true;
      if (!externalReason) externalReason = 'TypeScript Program loaded a source outside the captured project and resolution closure';
      rejectJSQualification('TypeScript Program loaded a source outside the captured project and resolution closure');
      continue;
    }
    if (owners.size !== 1) {
      staticTypeScriptModuleClosureComplete = false;
      markReferenceIncomplete('a compiler source belongs to multiple captured project scopes');
      continue;
    }
    const owner = owners.values().next().value;
    if (owner === scopeID && !projectFiles.has(key)) {
      staticTypeScriptModuleClosureComplete = false;
      markReferenceIncomplete('a reachable captured source is omitted from the TypeScript config root set');
    }
    const file = fileByPath.get(key);
    if (file && file.generated) {
      staticTypeScriptModuleClosureComplete = false;
      markReferenceIncomplete('generated source has no exhaustive range-level reference proof');
    }
    if (owner !== scopeID || !projectFiles.has(key) || !typescriptSourceExtensions.has(path.extname(key).toLowerCase())) {
      staticTypeScriptModuleClosureComplete = false;
    }
  }
  // Unresolved names and diagnostics can make the compiler's symbol graph
  // smaller than the source's actual reference graph. Keep that scope partial
  // even when every emitted occurrence happened to receive an anchor.
  try {
    const diagnostics = program.getSyntacticDiagnostics().concat(program.getSemanticDiagnostics());
    if (jsQualificationRequested) {
      diagnostics.push(...program.getOptionsDiagnostics(), ...program.getGlobalDiagnostics());
    }
    if (diagnostics.some((diagnostic) => diagnostic.category === ts.DiagnosticCategory.Error)) {
      programHasErrors = true;
      markReferenceIncomplete('TypeScript Program has compiler errors that may hide reference targets');
    }
  } catch (_) {
    programHasErrors = true;
    markReferenceIncomplete('TypeScript Program diagnostics could not be enumerated');
  }
  const scopeFile = (sourceFile) => {
    if (!sourceFile) return null;
    const key = pathKey(sourceFile.fileName);
    if (!projectFiles.has(key)) return null;
    const file = fileByPath.get(key);
    if (!file) {
      externalFacts = true;
      return null;
    }
    knownSourceFiles.add(key);
    if (file.generated) generatedFacts = true;
    return file;
  };
  const checkedDirectiveFiles = new Set();
  const hasTypeScriptSuppression = (sourceFile) => {
    try {
      const scanner = ts.createScanner(ts.ScriptTarget.Latest, false, ts.LanguageVariant.Standard, sourceFile.text);
      for (let token = scanner.scan(); token !== ts.SyntaxKind.EndOfFileToken; token = scanner.scan()) {
        if (token !== ts.SyntaxKind.SingleLineCommentTrivia && token !== ts.SyntaxKind.MultiLineCommentTrivia) continue;
        if (/@ts-(?:ignore|expect-error|nocheck)\b/i.test(scanner.getTokenText())) return true;
      }
    } catch (_) {
      return true;
    }
    return false;
  };
  const anyUnknownFlags = ts.TypeFlags.Any | ts.TypeFlags.Unknown;
  const hasAnyOrUnknown = (initialType) => {
    const seen = new Set();
    const pendingTypes = [initialType];
    while (pendingTypes.length && seen.size < 256) {
      const type = pendingTypes.pop();
      if (!type || seen.has(type)) continue;
      seen.add(type);
      if (type.flags & anyUnknownFlags) return true;
      if (type.types) pendingTypes.push(...type.types);
      if (type.aliasTypeArguments) pendingTypes.push(...type.aliasTypeArguments);
      if ((type.objectFlags & ts.ObjectFlags.Reference) !== 0) {
        try {
          pendingTypes.push(...checker.getTypeArguments(type));
        } catch (_) {
          return true;
        }
      }
    }
    return false;
  };
  const hasAnyOrUnknownAt = (node) => {
    try {
      return hasAnyOrUnknown(checker.getTypeAtLocation(node));
    } catch (_) {
      return true;
    }
  };
  const hasStringTypeAt = (node) => {
    try {
      const type = checker.getTypeAtLocation(node);
      const includesString = (candidate) => {
        if (!candidate) return false;
        if (candidate.flags & ts.TypeFlags.StringLike) return true;
        return typeof candidate.isUnion === 'function' && candidate.isUnion() && candidate.types.some(includesString);
      };
      return includesString(type);
    } catch (_) {
      return true;
    }
  };
  const runtimeEscapeNames = new Set([
    'require', 'eval', 'Function', 'AsyncFunction', 'GeneratorFunction', 'AsyncGeneratorFunction',
    'Proxy', 'Reflect', 'Object', 'globalThis', 'window', 'self', 'global', 'prototype', '__proto__',
    'setTimeout', 'setInterval', 'setImmediate', 'execScript', 'runInThisContext', 'runInNewContext',
    'runInContext', 'compileFunction', 'vm',
  ]);
  const moduleRuntimeEscapeNames = new Set([
    'require', 'eval', 'Function', 'AsyncFunction', 'GeneratorFunction', 'AsyncGeneratorFunction',
    'createRequire', 'execScript', 'runInThisContext', 'runInNewContext', 'runInContext', 'compileFunction',
  ]);
  const moduleRuntimeEscapeReasonForName = (name) => {
    if (name === 'require' || name === 'eval') {
      return 'require and eval can construct module loads outside static import syntax';
    }
    if (name === 'Function' || name === 'AsyncFunction' || name === 'GeneratorFunction' || name === 'AsyncGeneratorFunction' || name === 'constructor') {
      return 'Function constructors and indirect constructor access can construct module loads outside static import syntax';
    }
    return 'runtime module loaders and code-generation APIs can hide module loads from static imports';
  };
  const stringCodeExecutionNames = new Set(['setTimeout', 'setInterval', 'setImmediate']);
  const reflectivePropertyNames = new Set([
    'constructor', 'defineProperty', 'defineProperties', 'setPrototypeOf', 'getPrototypeOf',
    'getOwnPropertyDescriptor', 'getOwnPropertyDescriptors', 'getOwnPropertyNames',
    'getOwnPropertySymbols', 'ownKeys', '__defineGetter__', '__defineSetter__',
  ]);
  const isConstructorPropertyName = (node) => {
    if (!node || node.text !== 'constructor') return false;
    const parent = node.parent;
    if (!parent) return false;
    return (ts.isPropertyAccessExpression(parent) && parent.name === node) ||
      (ts.isPropertyAssignment(parent) && parent.name === node) ||
      (ts.isPropertyDeclaration(parent) && parent.name === node) ||
      (ts.isPropertySignature(parent) && parent.name === node) ||
      (ts.isMethodDeclaration(parent) && parent.name === node) ||
      (ts.isMethodSignature(parent) && parent.name === node) ||
      (ts.isBindingElement(parent) && parent.propertyName === node);
  };
  const isConstructorPropertyAccess = (node) => {
    if (!node || node.text !== 'constructor') return false;
    const parent = node.parent;
    return !!parent && (
      (ts.isPropertyAccessExpression(parent) && parent.name === node) ||
      (ts.isBindingElement(parent) && parent.propertyName === node)
    );
  };
  const isTypeOnlyName = (node) => {
    let current = node;
    for (let parent = node && node.parent; parent; current = parent, parent = parent.parent) {
      if ((ts.isTypeReferenceNode(parent) && parent.typeName === current) ||
          (ts.isTypeQueryNode(parent) && parent.exprName === current)) return true;
      if (!ts.isQualifiedName(parent)) return false;
    }
    return false;
  };
  const inspectJavaScriptNode = (node) => {
    if (!jsQualificationRequested) return;
    if (node.kind === ts.SyntaxKind.JsxElement || node.kind === ts.SyntaxKind.JsxSelfClosingElement ||
        node.kind === ts.SyntaxKind.JsxFragment) {
      rejectJSQualification('JSX syntax is outside the static checkJs profile');
    }
    if (ts.isIdentifier(node)) {
      if (runtimeEscapeNames.has(node.text) || isConstructorPropertyName(node)) {
        rejectJSQualification('runtime, prototype, or reflective JavaScript escape is outside the static checkJs profile');
      }
    }
    if (ts.isPropertyAccessExpression(node) && reflectivePropertyNames.has(node.name.text)) {
      rejectJSQualification('runtime, prototype, or reflective JavaScript escape is outside the static checkJs profile');
    }
    if (ts.isElementAccessExpression(node) && node.argumentExpression && ts.isStringLiteralLike(node.argumentExpression) &&
        reflectivePropertyNames.has(node.argumentExpression.text)) {
      rejectJSQualification('runtime, prototype, or reflective JavaScript escape is outside the static checkJs profile');
    }
    if (ts.isStringLiteralLike(node) && node.parent &&
        ((ts.isElementAccessExpression(node.parent) && node.parent.argumentExpression === node) ||
         (ts.isBindingElement(node.parent) && node.parent.propertyName === node)) &&
        (runtimeEscapeNames.has(node.text) || reflectivePropertyNames.has(node.text))) {
      rejectJSQualification('runtime, prototype, or reflective JavaScript escape is outside the static checkJs profile');
    }
    if ((ts.isSpreadAssignment(node) || ts.isSpreadElement(node)) && hasAnyOrUnknownAt(node.expression)) {
      rejectJSQualification('any or unknown type reaches a JavaScript spread');
    }
    if (ts.isPropertyAccessExpression(node)) {
      if (hasAnyOrUnknownAt(node.expression) || hasAnyOrUnknownAt(node)) {
        rejectJSQualification('any or unknown type reaches a JavaScript member access');
      }
    } else if (ts.isElementAccessExpression(node)) {
      if (hasAnyOrUnknownAt(node.expression) || hasAnyOrUnknownAt(node) ||
          (node.argumentExpression && hasAnyOrUnknownAt(node.argumentExpression))) {
        rejectJSQualification('any or unknown type reaches a JavaScript member access');
      }
    } else if (ts.isCallExpression(node) || ts.isNewExpression(node)) {
      if (hasAnyOrUnknownAt(node.expression) || hasAnyOrUnknownAt(node) ||
          (node.arguments || []).some(hasAnyOrUnknownAt)) {
        rejectJSQualification('any or unknown type reaches a JavaScript call');
      }
    }
  };
  const noteUnresolved = (symbol) => {
    if (!symbol) {
      if (jsQualificationRequested) {
        for (const fact of ['symbol', 'declaration', 'definition', 'reference']) {
          markIncomplete(fact, 'a JavaScript symbol occurrence has no compiler-resolved declaration anchor');
        }
        return;
      }
      externalFacts = true;
      if (!externalReason) externalReason = 'missing compiler symbol';
      return;
    }
    const declarations = typeof symbol.getDeclarations === 'function' ? symbol.getDeclarations() : symbol.declarations;
    if (!declarations || !declarations.length) {
      if (jsQualificationRequested) {
        for (const fact of ['symbol', 'declaration', 'definition', 'reference']) {
          markIncomplete(fact, 'a JavaScript symbol has no compiler declaration anchor');
        }
      }
      return;
    }
    const hasProjectDeclaration = declarations.some((declaration) => {
      const sourceFile = declaration.getSourceFile();
      return contextByFile.has(pathKey(sourceFile.fileName)) && fileByPath.has(pathKey(sourceFile.fileName));
    });
    if (!hasProjectDeclaration && !declarations.every((declaration) => isCompilerSource(declaration.getSourceFile()))) {
      externalFacts = true;
      if (!externalReason) externalReason = symbol.getName() + ' declaration outside project';
    }
  };
  const resolvedSymbol = (symbol) => {
    if (!symbol) return null;
    try {
      if ((symbol.flags & ts.SymbolFlags.Alias) !== 0) {
        const aliased = checker.getAliasedSymbol(symbol);
        const declarations = aliased && typeof aliased.getDeclarations === 'function' ? aliased.getDeclarations() : aliased && aliased.declarations;
        // ImportEquals and namespace imports have their own local binding.
        // TypeScript's reference search does not treat uses of that binding as
        // references to the source-file or export-assignment symbol itself.
        if (aliased && !(declarations || []).some((declaration) =>
          declaration.kind === ts.SyntaxKind.SourceFile || ts.isExportAssignment(declaration))) symbol = aliased;
      }
    } catch (_) {
      noteUnresolved(symbol);
      return null;
    }
    return symbol;
  };
  const unanchoredSymbolReason = (rawSymbol, fallback) => {
    const symbol = resolvedSymbol(rawSymbol);
    if (!symbol) return fallback;
    const declarations = typeof symbol.getDeclarations === 'function' ? symbol.getDeclarations() : symbol.declarations;
    if (declarations && declarations.length && declarations.every((declaration) => {
      const sourceFile = declaration.getSourceFile();
      return sourceFile.isDeclarationFile && isCompilerSource(sourceFile);
    })) {
      return 'TypeScript standard-library declarations are outside the immutable project manifest';
    }
    return fallback;
  };
  const declarationKey = (declaration) => {
    const sourceFile = declaration.getSourceFile();
    const file = fileByPath.get(pathKey(sourceFile.fileName));
    if (!file) return '';
    let start;
    try {
      start = declaration.name ? declaration.name.getStart(sourceFile) : declaration.getStart(sourceFile);
    } catch (_) {
      return '';
    }
    return file.uri + '#' + String(start);
  };
  const symbolID = (rawSymbol) => {
    const symbol = resolvedSymbol(rawSymbol);
    if (!symbol) return null;
    // The Map key is the actual compiler Symbol object, so aliases and
    // declarations are coalesced only when TypeChecker returns the same
    // Program identity. The serialized ID is a stable declaration anchor for
    // persistence; it is never inferred from an identifier's spelling alone.
    if (symbolIDs.has(symbol)) return symbolIDs.get(symbol);
    const declarations = typeof symbol.getDeclarations === 'function' ? symbol.getDeclarations() : symbol.declarations;
    const usable = (declarations || []).map(declarationKey).filter(Boolean).sort();
    if (!usable.length) {
      noteUnresolved(symbol);
      return null;
    }
    const declarationContexts = new Set();
    for (const declaration of declarations || []) {
      const sourceFile = declaration.getSourceFile();
      const key = pathKey(sourceFile.fileName);
      const context = contextByFile.get(key);
      if (!context || ambiguousDeclarationFiles.has(key)) {
        markIncomplete('symbol', 'declaration belongs to an unowned or multiply-owned project-reference source');
        return null;
      }
      declarationContexts.add(context);
    }
    if (declarationContexts.size !== 1) {
      markIncomplete('symbol', 'declaration is merged across project-reference build contexts');
      return null;
    }
    const declarationContext = declarationContexts.values().next().value;
    // Reference declarations retain the target project's build context so
    // they link to the same symbol emitted by that project's own Program.
    const id = 'typescript/6.0.3/' + declarationContext + '/' + usable.join('|');
    const first = (declarations || []).find((declaration) => declarationKey(declaration) === usable[0]) || declarations[0];
    const sourceFileDeclaration = (declarations || []).find((declaration) => declaration.kind === ts.SyntaxKind.SourceFile);
    const moduleGraphFile = sourceFileDeclaration ? fileByPath.get(pathKey(sourceFileDeclaration.fileName)) : null;
    if (sourceFileDeclaration && !moduleGraphFile) {
      markIncomplete('symbol', 'SourceFile module identity has no captured logical URI');
      return null;
    }
    symbolIDs.set(symbol, id);
    let signature = '';
    try {
      if (first && ts.isFunctionLike(first)) {
        const value = checker.getSignatureFromDeclaration(first);
        if (value) signature = checker.signatureToString(value, first, ts.TypeFormatFlags.NoTruncation);
      }
    } catch (_) {
      signature = '';
    }
    const value = {
      ID: id,
      // TypeScript gives SourceFile module symbols a name derived from their
      // physical file path. The compiler runs over a temporary materialized
      // project, so that name is unstable and can also leak the temp path into
      // workspace-symbol queries. Use the captured logical URI for this graph
      // node while retaining its compiler-derived ID and all graph edges.
      Name: moduleGraphFile ? moduleGraphFile.uri : symbol.getName(),
      Kind: moduleGraphFile ? 'module_graph_node' : syntaxKindName(ts, first),
      Signature: signature,
    };
    queueFact('Symbols', value);
    return id;
  };
  const addOccurrence = (node, rawSymbol, role, occurrenceSpan) => {
    const file = scopeFile(node && node.getSourceFile ? node.getSourceFile() : null);
    if (!file) return null;
    const id = symbolID(rawSymbol);
    if (!id) {
      markOccurrenceIncomplete(role, unanchoredSymbolReason(rawSymbol, 'source occurrence could not be anchored to a project declaration'));
      return null;
    }
    let range;
    try {
      const sourceFile = node.getSourceFile();
      const start = occurrenceSpan ? occurrenceSpan.start : node.getStart(sourceFile);
      const end = occurrenceSpan ? occurrenceSpan.end : node.getEnd();
      if (end <= start) {
        markOccurrenceIncomplete(role, 'source occurrence has no representable non-empty range');
        return id;
      }
      range = sourcePosition(sourceFile, { start, end });
    } catch (_) {
      markOccurrenceIncomplete(role, 'source occurrence range could not be computed');
      return id;
    }
    // Each source AST node is visited once; a node contributes at most one
    // occurrence for a given role, so a project-sized global dedupe set is
    // unnecessary and would defeat bounded output memory.
    queueFact('Occurrences', { SymbolID: id, URI: file.uri, Range: range, Role: role });
    return id;
  };
  const markOccurrenceIncomplete = (role, reason) => {
    const fact = role === 'reference' ? 'reference' : role;
    markIncomplete(fact, reason);
    if (role !== 'reference') markIncomplete('symbol', reason);
  };
  const literalContentSpan = (node) => {
    const sourceFile = node.getSourceFile();
    return { start: node.getStart(sourceFile) + 1, end: node.getEnd() - 1 };
  };
  const sourceModuleID = (sourceFile) => {
    try {
      return symbolID(checker.getSymbolAtLocation(sourceFile));
    } catch (_) {
      return null;
    }
  };
  const scriptScopeIDs = new Map();
  const syntheticScriptScopeID = (sourceFile) => {
    const key = pathKey(sourceFile.fileName);
    if (scriptScopeIDs.has(key)) return scriptScopeIDs.get(key);
    const file = fileByPath.get(key);
    const buildContext = contextByFile.get(key);
    const sourceHash = file && typeof file.sourceHash === 'string' ? file.sourceHash.toLowerCase() : '';
    if (!file || !buildContext || ambiguousDeclarationFiles.has(key) || !/^sha256:[0-9a-f]{64}$/.test(sourceHash)) {
      markExternal('script scope could not be bound to a captured source revision and build context');
      return null;
    }
    // Plain script files have no TypeScript module Symbol to own top-level
    // calls. Give that compiler scope an explicit synthetic identity bound to
    // the logical URI, captured source hash, and owning project context.
    const uriDigest = crypto.createHash('sha256').update(file.uri, 'utf8').digest('hex');
    const id = 'typescript/6.0.3/' + buildContext + '/script-scope:sha256:' + uriDigest + ':revision:' + sourceHash;
    scriptScopeIDs.set(key, id);
    queueFact('Symbols', { ID: id, Name: '<script scope>', Kind: 'synthetic_script_scope', Signature: '' });
    return id;
  };
  const sourceContainerID = (sourceFile) => {
    if (ts.isExternalModule(sourceFile) || sourceFile.commonJsModuleIndicator) {
      const id = sourceModuleID(sourceFile);
      if (!id) markIncomplete('call', 'TypeScript module source has no compiler module identity');
      return id;
    }
    return syntheticScriptScopeID(sourceFile);
  };
  const ownerID = (node) => {
    for (let current = node.parent; current; current = current.parent) {
      if (current.name && ts.isIdentifier(current.name)) {
        const id = symbolID(checker.getSymbolAtLocation(current.name));
        if (id) return id;
      }
    }
    return sourceContainerID(node.getSourceFile());
  };
  const addEdge = (node, from, to, kind) => {
    const file = scopeFile(node && node.getSourceFile ? node.getSourceFile() : null);
    if (!file || !from || !to) return;
    let range;
    try {
      const sourceFile = node.getSourceFile();
      range = sourcePosition(sourceFile, { start: node.getStart(sourceFile), end: node.getEnd() });
    } catch (_) {
      markIncomplete(kind, 'semantic edge range could not be computed');
      return;
    }
    // Every edge is tied to one distinct call/import/heritage/type AST node.
    // Preserve duplicate semantic edges at distinct syntax sites.
    queueFact('Edges', { From: from, To: to, Kind: kind, SourceURI: file.uri, Range: range });
  };
  const callTarget = (node) => {
    try {
      const signature = checker.getResolvedSignature(node);
      if (signature && signature.declaration) {
        const declaration = signature.declaration;
        const direct = checker.getSymbolAtLocation(declaration.name || declaration);
        if (direct) return direct;
      }
      if (ts.isNewExpression(node)) {
        // Constructor signatures point at ConstructorDeclaration, whose
        // syntax node has no name. The expression's symbol is the compiler's
        // class/constructor identity for that same Program.
        const expression = checker.getSymbolAtLocation(node.expression);
        if (expression) return expression;
        const type = checker.getTypeAtLocation(node.expression);
        if (type && type.symbol) return type.symbol;
      }
    } catch (_) {
      return null;
    }
    return null;
  };

  const hasAmbientModifier = (declaration) => {
    if (!declaration || declaration.getSourceFile().isDeclarationFile) return true;
    try {
      return (ts.getCombinedModifierFlags(declaration) & ts.ModifierFlags.Ambient) !== 0;
    } catch (_) {
      return false;
    }
  };
  const hasDefinitionBody = (declaration) => {
    if (!declaration || hasAmbientModifier(declaration)) return false;
    if (ts.isFunctionLike(declaration)) return !!declaration.body;
    if (ts.isClassDeclaration(declaration) || ts.isClassExpression(declaration) || ts.isEnumDeclaration(declaration)) return true;
    if (ts.isModuleDeclaration(declaration)) return !!declaration.body && ts.isModuleBlock(declaration.body);
    if (ts.isVariableDeclaration(declaration) || ts.isPropertyDeclaration(declaration)) return !!declaration.initializer;
    if (ts.isBindingElement(declaration)) {
      for (let current = declaration.parent; current; current = current.parent) {
        if (ts.isVariableDeclaration(current)) return !!current.initializer && !hasAmbientModifier(current);
        if (ts.isFunctionLike(current)) return !!current.body && !hasAmbientModifier(current);
      }
    }
    return false;
  };
  const hasClosedCallImplementation = (symbol) => {
    const declarations = typeof symbol.getDeclarations === 'function' ? symbol.getDeclarations() : symbol.declarations;
    if (!declarations || !declarations.length) return false;
    const projectDeclarations = declarations.filter((declaration) => !isCompilerSource(declaration.getSourceFile()));
    // A declaration from TypeScript's library files describes the type but
    // does not prove what its runtime implementation can load or execute.
    if (!projectDeclarations.length) return false;
    return projectDeclarations.every((declaration) => projectFiles.has(pathKey(declaration.getSourceFile().fileName))) &&
      projectDeclarations.some(hasDefinitionBody);
  };
  const addTypeRelation = (node, target, reason) => {
    const owner = ownerID(node);
    const targetID = target ? symbolID(target) : null;
    if (owner && targetID) {
      addEdge(node, owner, targetID, 'type_relation');
      return;
    }
    if (target) noteUnresolved(target);
    markTypeRelationIncomplete(unanchoredSymbolReason(target, reason || 'explicit type target or owner is outside the materialized project'));
  };
  const importTypeSpecifier = (node) => {
    if (!ts.isImportTypeNode(node) || !ts.isLiteralTypeNode(node.argument)) return null;
    const literal = node.argument.literal;
    return ts.isStringLiteralLike(literal) ? literal : null;
  };
  const isImportTypeSpecifier = (node) => node.parent && ts.isLiteralTypeNode(node.parent) &&
    ts.isImportTypeNode(node.parent.parent) && node.parent.literal === node;
  const contextualPropertyReference = (node) => {
    const element = node.parent;
    if (!(ts.isPropertyAssignment(element) || ts.isShorthandPropertyAssignment(element)) || element.name !== node) return null;
    try {
      const contextualType = checker.getContextualType(element.parent);
      return contextualType ? checker.getPropertyOfType(contextualType, node.text) : null;
    } catch (_) {
      return null;
    }
  };
  const aliasTargetReference = (node, symbol) => {
    const parent = node.parent;
    if ((ts.isImportSpecifier(parent) && parent.name === node) ||
        (ts.isImportClause(parent) && parent.name === node)) return symbol;
    if (ts.isExportSpecifier(parent) && parent.name === node) {
      try {
        return checker.getExportSpecifierLocalTargetSymbol(parent) || symbol;
      } catch (_) {
        return symbol;
      }
    }
    return null;
  };
  const isLiteralMemberName = (node) => {
    const parent = node.parent;
    if (!parent) return false;
    if ((ts.isPropertyAssignment(parent) || ts.isPropertyDeclaration(parent) || ts.isPropertySignature(parent) ||
         ts.isMethodDeclaration(parent) || ts.isMethodSignature(parent) || ts.isGetAccessorDeclaration(parent) ||
         ts.isSetAccessorDeclaration(parent) || ts.isEnumMember(parent) || ts.isModuleDeclaration(parent)) && parent.name === node) return true;
    if (ts.isBindingElement(parent) && (parent.name === node || parent.propertyName === node)) return true;
    if ((ts.isImportSpecifier(parent) || ts.isExportSpecifier(parent)) && (parent.name === node || parent.propertyName === node)) return true;
    return false;
  };
  const supportedDeclarationName = (node) => {
    const parent = node.parent;
    if (!parent) return false;
    return ts.isVariableDeclaration(parent) || ts.isFunctionDeclaration(parent) || ts.isFunctionExpression(parent) ||
      ts.isClassDeclaration(parent) || ts.isClassExpression(parent) || ts.isInterfaceDeclaration(parent) ||
      ts.isTypeAliasDeclaration(parent) || ts.isEnumDeclaration(parent) || ts.isEnumMember(parent) ||
      ts.isModuleDeclaration(parent) || ts.isPropertyDeclaration(parent) || ts.isPropertySignature(parent) ||
      ts.isPropertyAssignment(parent) || ts.isShorthandPropertyAssignment(parent) ||
      ts.isMethodDeclaration(parent) || ts.isMethodSignature(parent) ||
      ts.isGetAccessorDeclaration(parent) || ts.isSetAccessorDeclaration(parent) || ts.isParameter(parent) ||
      ts.isTypeParameterDeclaration(parent) || ts.isBindingElement(parent) || ts.isImportClause(parent) ||
      ts.isImportSpecifier(parent) || ts.isNamespaceImport(parent) || ts.isImportEqualsDeclaration(parent) ||
      ts.isExportSpecifier(parent);
  };

  const unsupportedJSDocTags = new Set(['callback', 'enum', 'import', 'template', 'typedef']);
  const visit = (node, inJSDoc = false) => {
    const file = scopeFile(node.getSourceFile());
    if (!file) return;
    if (jsQualificationRequested) {
      const sourceFile = node.getSourceFile();
      const sourceKey = pathKey(sourceFile.fileName);
      if (!checkedDirectiveFiles.has(sourceKey)) {
        checkedDirectiveFiles.add(sourceKey);
        if (hasTypeScriptSuppression(sourceFile)) {
          rejectJSQualification('TypeScript suppression directives are outside the static checkJs profile');
        }
      }
      inspectJavaScriptNode(node);
    }
    // JSDoc tag names can resolve to the associated parameter symbol, but
    // they are syntax labels rather than references (for example, "param").
    if (inJSDoc && node.parent && ts.isJSDocTag(node.parent) && node.parent.tagName === node) return;
    if (inJSDoc && ts.isJSDocTag(node) && node.tagName) {
      const tagName = node.tagName.text || node.tagName.escapedText;
      if (unsupportedJSDocTags.has(String(tagName))) {
        const reason = 'JSDoc declaration and import tags are outside the proven static semantic visitor';
        for (const fact of ['symbol', 'declaration', 'definition', 'reference', 'type_relation']) {
          markIncomplete(fact, reason);
        }
        return;
      }
    }
    // TypeScript stores JSDoc as side data rather than ordinary forEachChild
    // edges. Visit that compiler AST explicitly so checker-resolved types,
    // parameter names, and links contribute their actual source references.
    if (node.jsDoc && node.jsDoc.length) {
      for (const doc of node.jsDoc) visit(doc, true);
    }
    // A project can shadow CommonJS/eval globals with local declarations, so
    // symbol resolution alone cannot prove that these names are static. Fence
    // the identifiers even when they are only captured into an alias; the
    // alias call itself may no longer mention the dynamic loader by name.
    if (ts.isIdentifier(node)) {
      if (isDynamicRuntimeName(node.text)) {
        dynamicFacts = true;
        markModuleRuntimeEscape('require and eval can construct module loads outside static import syntax');
      } else if ((moduleRuntimeEscapeNames.has(node.text) && !isTypeOnlyName(node)) || isConstructorPropertyAccess(node)) {
        markModuleRuntimeEscape(moduleRuntimeEscapeReasonForName(isConstructorPropertyAccess(node) ? 'constructor' : node.text));
      }
    }
    if (ts.isStringLiteralLike(node) && node.parent &&
        ((ts.isElementAccessExpression(node.parent) && node.parent.argumentExpression === node) ||
         (ts.isBindingElement(node.parent) && node.parent.propertyName === node))) {
      if (isDynamicRuntimeName(node.text)) {
        dynamicFacts = true;
        markModuleRuntimeEscape('require and eval can construct module loads outside static import syntax');
      } else if (moduleRuntimeEscapeNames.has(node.text) || node.text === 'constructor') {
        markModuleRuntimeEscape(moduleRuntimeEscapeReasonForName(node.text));
      }
    }
    if (ts.isDecorator(node) || node.kind === ts.SyntaxKind.WithStatement || node.kind === ts.SyntaxKind.DeleteExpression) {
      markReferenceIncomplete('decorators, with-scopes, and delete operations are outside the proven static reference profile');
    }
    if (ts.isComputedPropertyName(node)) {
      markReferenceIncomplete('computed member declarations are outside the proven static reference profile');
    }
    if ((ts.isStringLiteralLike(node) || ts.isNumericLiteral(node)) && isLiteralMemberName(node)) {
      markReferenceIncomplete('string and numeric member-name declarations are outside the proven static reference profile');
    }
    if ((ts.isStringLiteralLike(node) || ts.isNumericLiteral(node)) && node.parent && ts.isLiteralTypeNode(node.parent) &&
        node.parent.parent && ts.isIndexedAccessTypeNode(node.parent.parent) && node.parent.parent.indexType === node.parent) {
      markReferenceIncomplete('literal indexed-access types are outside the proven static TypeScript reference profile');
    }
    if (ts.isIdentifier(node) || ts.isPrivateIdentifier(node)) {
      // TypeScript classifies a shorthand property name as a declaration
      // name, although it simultaneously denotes the initializer's value and
      // a contextual property reference. Treat it only through those semantic
      // reference paths below; the transient shorthand property symbol has no
      // declaration identity of its own.
      const shorthandPropertyName = ts.isShorthandPropertyAssignment(node.parent) && node.parent.name === node;
      const contextualProperty = contextualPropertyReference(node);
      const declaration = ts.isDeclarationName(node) && !shorthandPropertyName && !contextualProperty;
      if (declaration && !supportedDeclarationName(node)) {
        markReferenceIncomplete('declaration-name syntax is outside the proven static TypeScript reference profile');
      }
      if (declaration && ts.isParameter(node.parent) && (node.parent.modifiers || []).some((modifier) =>
        modifier.kind === ts.SyntaxKind.PublicKeyword || modifier.kind === ts.SyntaxKind.PrivateKeyword ||
        modifier.kind === ts.SyntaxKind.ProtectedKeyword || modifier.kind === ts.SyntaxKind.ReadonlyKeyword)) {
        markReferenceIncomplete('constructor parameter properties have distinct parameter and property symbols');
      }
      const symbol = checker.getSymbolAtLocation(node);
      const shorthandValue = shorthandPropertyName
        ? checker.getShorthandAssignmentValueSymbol(node.parent)
        : null;
      if (symbol && declaration) {
        addOccurrence(node, symbol, 'declaration');
        if (hasDefinitionBody(node.parent)) addOccurrence(node, symbol, 'definition');
        const aliasTarget = aliasTargetReference(node, symbol);
        if (aliasTarget) addOccurrence(node, aliasTarget, 'reference');
        if (shorthandValue) addOccurrence(node, shorthandValue, 'reference');
        if (contextualProperty) addOccurrence(node, contextualProperty, 'reference');
        if (ts.isBindingElement(node.parent) && node.parent.name === node && !node.parent.propertyName) {
          let property = null;
          try {
            const patternType = checker.getTypeAtLocation(node.parent.parent);
            if (patternType) property = checker.getPropertyOfType(patternType, node.text);
          } catch (_) {
            property = null;
          }
          if (property) addOccurrence(node, property, 'reference');
          else markIncomplete('reference', 'destructuring binding property has no compiler-resolved symbol');
        }
      } else if (symbol || shorthandValue) {
        const referenceSymbol = shorthandValue || (contextualProperty ? null : symbol);
        if (referenceSymbol) addOccurrence(node, referenceSymbol, 'reference');
        if (contextualProperty && contextualProperty !== shorthandValue) addOccurrence(node, contextualProperty, 'reference');
      }
      else if (!declaration) noteUnresolved(symbol);
      else {
        markIncomplete('symbol', 'declaration name has no TypeScript symbol');
        markIncomplete('declaration', 'declaration name has no TypeScript symbol');
        if (hasDefinitionBody(node.parent)) markIncomplete('definition', 'definition name has no TypeScript symbol');
      }
    } else if (ts.isStringLiteralLike(node) && node.parent && (
      ts.isImportDeclaration(node.parent) || ts.isExportDeclaration(node.parent) ||
      (ts.isImportEqualsDeclaration(node.parent) && node.parent.moduleReference === node) ||
      (ts.isExternalModuleReference(node.parent) && node.parent.parent &&
        ts.isImportEqualsDeclaration(node.parent.parent)) ||
      isImportTypeSpecifier(node) ||
      (ts.isCallExpression(node.parent) && node.parent.expression.kind === ts.SyntaxKind.ImportKeyword && node.parent.arguments[0] === node))) {
      const symbol = checker.getSymbolAtLocation(node);
      if (symbol) addOccurrence(node, symbol, 'reference', literalContentSpan(node));
      else noteUnresolved(symbol);
    }
    if (ts.isElementAccessExpression(node)) {
      const argument = node.argumentExpression;
      if (!argument) {
        markIncomplete('reference', 'element access has no statically identifiable argument');
      } else if (ts.isStringLiteralLike(argument) || ts.isNumericLiteral(argument)) {
        let symbol = checker.getSymbolAtLocation(argument);
        if (!symbol) symbol = checker.getSymbolAtLocation(node);
        if (symbol) addOccurrence(argument, symbol, 'reference', ts.isStringLiteralLike(argument) ? literalContentSpan(argument) : undefined);
        else markIncomplete('reference', 'literal element access has no compiler-resolved property symbol');
      } else {
        markIncomplete('reference', 'computed element access may resolve dynamically and has no single static property reference');
      }
    }
    if (ts.isCallExpression(node) || ts.isNewExpression(node)) {
      const dynamicImport = ts.isCallExpression(node) && node.expression.kind === ts.SyntaxKind.ImportKeyword;
      const literalDynamicImport = dynamicImport && node.arguments.length === 1 && ts.isStringLiteralLike(node.arguments[0]);
      if (dynamicImport) {
        dynamicFacts = true;
        if (!literalDynamicImport) markModuleRuntimeEscape('a nonliteral dynamic import can load modules outside the captured static graph');
      }
      const callName = ts.isIdentifier(node.expression) ? node.expression.text :
        (ts.isPropertyAccessExpression(node.expression) ? node.expression.name.text : '');
      if (staticTypeScriptModuleCandidate && moduleRuntimeEscapeNames.has(callName)) {
        markModuleRuntimeEscape(moduleRuntimeEscapeReasonForName(callName));
      }
      if (staticTypeScriptModuleCandidate && ts.isPropertyAccessExpression(node.expression) && node.expression.name.text === 'constructor') {
        markModuleRuntimeEscape(moduleRuntimeEscapeReasonForName('constructor'));
      }
      if (staticTypeScriptModuleCandidate && stringCodeExecutionNames.has(callName) && (node.arguments || []).some(hasStringTypeAt)) {
        markModuleRuntimeEscape('string callbacks passed to timer APIs can execute module loads outside static import syntax');
      }
      const from = ownerID(node);
      const to = callTarget(node);
      const targetID = to ? symbolID(to) : null;
      if (staticTypeScriptModuleCandidate) {
        if (hasAnyOrUnknownAt(node.expression) || (node.arguments || []).some(hasAnyOrUnknownAt)) {
          markModuleRuntimeEscape('any or unknown call targets or arguments can hide runtime module loads');
        }
        if (to && !hasClosedCallImplementation(to) && !(dynamicImport && literalDynamicImport)) {
          markModuleRuntimeEscape('a call target without a body in the captured TypeScript project can hide runtime module loads');
        }
      }
      if (from && targetID) addEdge(node, from, targetID, 'call');
      else if (to) {
        noteUnresolved(to);
        markIncomplete('call', unanchoredSymbolReason(to, 'call target or owner has no project declaration anchor'));
      } else if (staticTypeScriptModuleCandidate) {
        const reason = 'call target or owner is unresolved: ' + node.getText();
        markIncomplete('call', reason);
        markModuleRuntimeEscape('a call target or owner could not be linked to a captured TypeScript implementation');
      }
      else if (jsQualificationRequested) {
        for (const fact of ['symbol', 'declaration', 'definition', 'reference']) {
          markIncomplete(fact, 'a JavaScript call target has no compiler declaration anchor');
        }
        unresolvedCallTargetReason = 'call target or owner is unresolved: ' + node.getText();
      } else if (!(staticTypeScriptModuleCandidate && moduleRuntimeEscapeReason)) {
        markExternal('call target or owner is unresolved: ' + node.getText());
      }
    }
    const importEqualsSpecifier = ts.isImportEqualsDeclaration(node) && ts.isExternalModuleReference(node.moduleReference)
      ? node.moduleReference.expression
      : null;
    const staticImportTypeSpecifier = importTypeSpecifier(node);
    const dynamicImportSpecifier = ts.isCallExpression(node) && node.expression.kind === ts.SyntaxKind.ImportKeyword &&
      node.arguments.length === 1 && ts.isStringLiteralLike(node.arguments[0]) ? node.arguments[0] : null;
    if (ts.isImportDeclaration(node) || ts.isExportDeclaration(node) || importEqualsSpecifier || staticImportTypeSpecifier || dynamicImportSpecifier) {
      const from = sourceModuleID(node.getSourceFile());
      const specifier = importEqualsSpecifier || staticImportTypeSpecifier || dynamicImportSpecifier || node.moduleSpecifier;
      if (!specifier || !ts.isStringLiteralLike(specifier)) {
        markIncomplete('module', 'module specifier is not a static string literal');
        markIncomplete('import', 'module specifier is not a static string literal');
        ts.forEachChild(node, visit);
        return;
      }
      const to = checker.getSymbolAtLocation(specifier);
      const targetID = to ? symbolID(to) : null;
      if (from && targetID) {
        addEdge(node, from, targetID, 'module');
        addEdge(node, from, targetID, 'import');
      } else if (to) {
        noteUnresolved(to);
        markIncomplete('module', 'module target or source has no project declaration anchor');
        markIncomplete('import', 'import target or source has no project declaration anchor');
      }
      else markExternal('module target or source is unresolved');
    }
    if (ts.isHeritageClause(node)) {
      const owner = node.parent && node.parent.name ? symbolID(checker.getSymbolAtLocation(node.parent.name)) : null;
      for (const type of node.types || []) {
        const target = checker.getSymbolAtLocation(type.expression);
        const targetID = target ? symbolID(target) : null;
        const kind = node.token === ts.SyntaxKind.ImplementsKeyword ? 'implementation' : 'type_relation';
        if (owner && targetID) addEdge(type, owner, targetID, kind);
        else if (target) {
          noteUnresolved(target);
          markIncomplete(kind, unanchoredSymbolReason(target, 'heritage target or owner has no project declaration anchor'));
        }
        else markExternal('heritage target or owner is unresolved');
      }
    }
    if (ts.isTypeReferenceNode(node)) {
      addTypeRelation(node, checker.getSymbolAtLocation(node.typeName), 'type reference target or owner is unresolved');
    } else if (ts.isTypeQueryNode(node)) {
      addTypeRelation(node, checker.getSymbolAtLocation(node.exprName), 'typeof type query target or owner is unresolved');
    } else if (ts.isImportTypeNode(node)) {
      let target = null;
      try {
        const type = checker.getTypeFromTypeNode(node);
        target = type.aliasSymbol || type.getSymbol();
      } catch (_) {
        target = null;
      }
      addTypeRelation(node, target, 'import type target or owner is unresolved');
    }
    ts.forEachChild(node, (child) => visit(child, inJSDoc));
  };

  for (const sourceFile of program.getSourceFiles()) {
    if (isCompilerSource(sourceFile)) continue;
    if (!projectFiles.has(pathKey(sourceFile.fileName))) continue;
    if (!scopeFile(sourceFile)) continue;
    visit(sourceFile);
  }
  flushFacts();
  if (generatedFacts) {
    dynamicFacts = true;
    rejectJSQualification('generated source has no exhaustive range-level reference proof');
  }
  if (dynamicFacts) rejectJSQualification('dynamic module or runtime behavior is outside the static compiler proof');
  const jsClosedSourceCount = Array.from(parsedScopes.values()).reduce((count, value) => count + value.projectFiles.size, 0);
  const jsQualificationProvenance = 'typescript-direct-js-checkjs-static-v1;typescript=6.0.3;allowJs=true;checkJs=true;closedScopes=' +
    String(parsedScopes.size) + ';sourceFiles=' + String(jsClosedSourceCount);
  const jsQualificationComplete = jsQualificationRequested && !jsQualificationFailure && !externalFacts &&
    !generatedFacts && knownSourceFiles.size > 0;
  const staticTypeScriptModuleProvenance = 'typescript-direct-ts-static-modules-v1;typescript=6.0.3;closedScopes=' +
    String(parsedScopes.size) + ';projectReferences=' + String((parsed.projectReferences || []).length) +
    ';sourceFiles=' + String(projectFiles.size);
  const staticTypeScriptModuleComplete = staticTypeScriptModuleClosureComplete && !programHasErrors &&
    !moduleRuntimeEscapeReason && !externalFacts && !generatedFacts && knownSourceFiles.size > 0 &&
    !incompleteReasons.import && !incompleteReasons.module;

  const coverage = [];
  for (const fact of FACTS) {
    let state = 'complete';
    let reason = '';
    if (fact === 'include') {
      state = 'unavailable';
      reason = 'preprocessor include relationships do not apply to TypeScript';
    } else if (!knownSourceFiles.size) {
      state = 'unknown';
      reason = 'TypeScript Program contained no source file from the immutable scope';
    } else if (externalFacts) {
      state = 'unknown';
      reason = baseReason + ': ' + (externalReason || 'a symbol or project reference resolved outside the materialized scope');
    } else if (generatedFacts) {
      state = 'incomplete_known_subset';
      reason = 'generated source was analyzed without an exhaustive range-level source-map proof';
    } else if (fact === 'call' && unresolvedCallTargetReason) {
      state = 'unknown';
      reason = unresolvedCallTargetReason;
    } else if (jsQualificationRequested && ['symbol', 'declaration', 'definition', 'reference'].includes(fact)) {
      if (jsQualificationComplete) {
        state = 'complete';
        reason = jsQualificationProvenance;
      } else {
        state = 'incomplete_known_subset';
        reason = jsQualificationFailure || 'the captured JavaScript project did not satisfy the static checkJs qualification';
      }
    } else if (incompleteReasons[fact]) {
      state = 'incomplete_known_subset';
      reason = fact === 'type_relation' ? (typeRelationReason || incompleteReasons[fact]) : incompleteReasons[fact];
    } else if ((fact === 'import' || fact === 'module') && moduleRuntimeEscapeReason) {
      state = 'incomplete_known_subset';
      reason = moduleRuntimeEscapeReason;
    } else if ((fact === 'import' || fact === 'module') && staticTypeScriptModuleComplete) {
      reason = staticTypeScriptModuleProvenance;
    } else if (dynamicFacts && ['symbol', 'declaration', 'definition', 'reference', 'implementation', 'type_relation', 'call', 'import', 'module'].includes(fact)) {
      state = 'incomplete_known_subset';
      reason = 'dynamic module or runtime behavior is outside the static compiler proof';
    }
    coverage.push({ ScopeID: scopeID, Fact: fact, State: state, Reason: reason });
  }
  return { batches: [], result: { Coverage: coverage } };
}

(async () => {
  let raw = '';
  let rawBytes = 0;
  for await (const chunk of process.stdin) {
    rawBytes += Buffer.isBuffer(chunk) ? chunk.length : Buffer.byteLength(String(chunk), 'utf8');
    if (rawBytes > MAX_REQUEST_BYTES) {
      send({ Type: 'error', Error: 'TypeScript compiler request exceeds the 128 MiB input budget; export unavailable' });
      return;
    }
    raw += chunk;
  }
  let request;
  try {
    request = JSON.parse(raw);
  } catch (error) {
    send({ Type: 'error', Error: 'invalid compiler request: ' + error.message });
    return;
  }
  try {
    const output = run(request);
    send({ Type: 'result', Result: output.result });
  } catch (error) {
    send({ Type: 'error', Error: error && error.stack ? error.stack : String(error) });
  }
})();
