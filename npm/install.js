#!/usr/bin/env node

"use strict";

const { execSync } = require("child_process");
const fs = require("fs");
const path = require("path");
const https = require("https");
const http = require("http");
const zlib = require("zlib");

const PACKAGE = require("./package.json");
const VERSION = `v${PACKAGE.version}`;
const NAME = "echo-client";
const LEGACY_NAME = "cc-connect";

const GITHUB_REPO = "neobay-io/echo-client";
const GITEE_REPO = "cg33/cc-connect";

const PLATFORM_MAP = {
  darwin: "darwin",
  linux: "linux",
  win32: "windows",
};

const ARCH_MAP = {
  x64: "amd64",
  arm64: "arm64",
};

function getPlatformInfo() {
  const platform = PLATFORM_MAP[process.platform];
  const arch = ARCH_MAP[process.arch];
  if (!platform || !arch) {
    throw new Error(
      `Unsupported platform: ${process.platform}/${process.arch}. ` +
        `Supported: linux/darwin/windows x64/arm64`
    );
  }
  const ext = platform === "windows" ? ".zip" : ".tar.gz";
  const githubFilename = `${NAME}-${VERSION}-${platform}-${arch}${ext}`;
  const giteeFilename = `${LEGACY_NAME}-${VERSION}-${platform}-${arch}${ext}`;
  return { platform, arch, ext, githubFilename, giteeFilename };
}

function getDownloadURLs(githubFilename, giteeFilename) {
  return [
    `https://github.com/${GITHUB_REPO}/releases/download/${VERSION}/${githubFilename}`,
    `https://gitee.com/${GITEE_REPO}/releases/download/${VERSION}/${giteeFilename}`,
  ];
}

function fetch(url, redirects = 5) {
  return new Promise((resolve, reject) => {
    if (redirects <= 0) return reject(new Error("Too many redirects"));
    const mod = url.startsWith("https") ? https : http;
    mod
      .get(url, { headers: { "User-Agent": "echo-client-npm" } }, (res) => {
        if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
          return resolve(fetch(res.headers.location, redirects - 1));
        }
        if (res.statusCode !== 200) {
          res.resume();
          return reject(new Error(`HTTP ${res.statusCode} for ${url}`));
        }
        const chunks = [];
        res.on("data", (c) => chunks.push(c));
        res.on("end", () => resolve(Buffer.concat(chunks)));
        res.on("error", reject);
      })
      .on("error", reject);
  });
}

async function download(urls) {
  for (const url of urls) {
    try {
      console.log(`[echo-client] Downloading from ${url}`);
      const data = await fetch(url);
      console.log(`[echo-client] Downloaded ${(data.length / 1024 / 1024).toFixed(1)} MB`);
      return data;
    } catch (err) {
      console.warn(`[echo-client] Failed: ${err.message}, trying next source...`);
    }
  }
  throw new Error(
    `[echo-client] Could not download binary from any source.\n` +
      `  Tried: ${urls.join(", ")}\n` +
      `  You can download manually from https://github.com/${GITHUB_REPO}/releases`
  );
}

function extractTarGz(buffer, destDir, binaryName) {
  const tmpFile = path.join(destDir, "_tmp.tar.gz");
  fs.writeFileSync(tmpFile, buffer);
  try {
    execSync(`tar xzf "${tmpFile}" -C "${destDir}"`, { stdio: "pipe" });
  } finally {
    fs.unlinkSync(tmpFile);
  }
  const extracted = fs.readdirSync(destDir).find((f) => (f.startsWith(NAME) || f.startsWith(LEGACY_NAME)) && !f.endsWith(".tar.gz"));
  if (extracted && extracted !== binaryName) {
    fs.renameSync(path.join(destDir, extracted), path.join(destDir, binaryName));
  }
}

function extractZip(buffer, destDir, binaryName) {
  const tmpFile = path.join(destDir, "_tmp.zip");
  fs.writeFileSync(tmpFile, buffer);
  try {
    try {
      execSync(`unzip -o "${tmpFile}" -d "${destDir}"`, { stdio: "pipe" });
    } catch {
      execSync(`powershell -Command "Expand-Archive -Force '${tmpFile}' '${destDir}'"`, {
        stdio: "pipe",
      });
    }
  } finally {
    try { fs.unlinkSync(tmpFile); } catch {}
  }
  const extracted = fs.readdirSync(destDir).find((f) => (f.startsWith(NAME) || f.startsWith(LEGACY_NAME)) && f.endsWith(".exe"));
  if (extracted && extracted !== binaryName) {
    fs.renameSync(path.join(destDir, extracted), path.join(destDir, binaryName));
  }
}

async function main() {
  const { platform, arch, ext, githubFilename, giteeFilename } = getPlatformInfo();
  console.log(`[echo-client] Platform: ${platform}/${arch}`);

  const binDir = path.join(__dirname, "bin");
  fs.mkdirSync(binDir, { recursive: true });

  const binaryName = platform === "windows" ? `${NAME}.exe` : NAME;
  const binaryPath = path.join(binDir, binaryName);

  if (fs.existsSync(binaryPath)) {
    try {
      const out = execSync(`"${binaryPath}" --version`, { encoding: "utf8", timeout: 5000 });
      if (out.includes(VERSION.slice(1))) {
        console.log(`[echo-client] Binary ${VERSION} already installed, skipping.`);
        return;
      }
      console.log(`[echo-client] Existing binary is outdated, upgrading to ${VERSION}...`);
      fs.unlinkSync(binaryPath);
    } catch {
      console.log(`[echo-client] Replacing existing binary with ${VERSION}...`);
      fs.unlinkSync(binaryPath);
    }
  }

  const urls = getDownloadURLs(githubFilename, giteeFilename);
  const data = await download(urls);

  if (ext === ".tar.gz") {
    extractTarGz(data, binDir, binaryName);
  } else {
    extractZip(data, binDir, binaryName);
  }

  if (platform !== "windows") {
    fs.chmodSync(binaryPath, 0o755);
  }

  if (platform === "darwin") {
    try {
      execSync(`xattr -d com.apple.quarantine "${binaryPath}"`, { stdio: "pipe" });
      console.log(`[echo-client] Removed macOS quarantine attribute`);
    } catch {
      // xattr fails if the attribute doesn't exist, which is fine
    }
  }

  console.log(`[echo-client] Installed to ${binaryPath}`);
}

main().catch((err) => {
  console.error(err.message);
  console.error(
    "[echo-client] Installation failed. You can install manually:\n" +
      `  https://github.com/${GITHUB_REPO}/releases/tag/${VERSION}`
  );
  process.exit(1);
});
