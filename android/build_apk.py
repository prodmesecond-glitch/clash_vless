#!/usr/bin/env python3
"""Build dist/clashvless.apk without Gradle: go -> aapt2 -> javac -> d8 -> zipalign -> apksigner.

Runs the same on Linux, macOS and Windows. Needs Go, a JDK 17+ and an Android SDK with
platforms;android-34 + build-tools;35.0.0. An NDK adds cgo (the system resolver) and the
armeabi-v7a and x86_64 builds; without one only a cgo-less arm64 library is built.
"""

import os
import re
import shutil
import subprocess
import sys
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
ANDROID = ROOT / "android"
APP = ROOT / "app"
DIST = ROOT / "dist"
OUT = ROOT / "tmp" / "android-build"
MIN_SDK = 24
TARGET_SDK = 34
PLATFORM = "android-34"
BUILD_TOOLS = "35.0.0"  # d8 from 34.0.0 crashes on MainActivity with a NullPointerException
EXE = ".exe" if sys.platform == "win32" else ""
# ABI, GOARCH, extra env, NDK compiler prefix.
ABIS = [
    ("arm64-v8a", "arm64", {}, "aarch64-linux-android"),
    ("armeabi-v7a", "arm", {"GOARM": "7"}, "armv7a-linux-androideabi"),
    ("x86_64", "amd64", {}, "x86_64-linux-android"),
]


def find_sdk():
    """The first SDK holding the pinned platform and build-tools."""
    home = Path.home()
    candidates = [os.environ.get("ANDROID_HOME"), os.environ.get("ANDROID_SDK_ROOT"),
                  home / "Android" / "Sdk", home / "Library" / "Android" / "sdk"]
    if os.environ.get("LOCALAPPDATA"):
        candidates.append(Path(os.environ["LOCALAPPDATA"]) / "Android" / "Sdk")
    for candidate in candidates:
        if candidate:
            sdk = Path(candidate)
            if (sdk / "platforms" / PLATFORM / "android.jar").is_file() and (sdk / "build-tools" / BUILD_TOOLS).is_dir():
                return sdk
    return None


def java_major(javac):
    try:
        out = subprocess.run([str(javac), "-version"], capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.TimeoutExpired):
        return 0
    match = re.search(r"javac (\d+)", out.stdout + out.stderr)
    return int(match.group(1)) if match else 0


def find_jdk():
    """The bin directory of a JDK 17+: JAVA_HOME, then ~/.jdks, then PATH."""
    homes = [os.environ.get("JAVA_HOME")] + sorted(str(p) for p in (Path.home() / ".jdks").glob("jdk-*"))
    for pf in (os.environ.get("ProgramFiles", r"C:\Program Files"),
               os.environ.get("ProgramFiles(x86)", r"C:\Program Files (x86)")):
        pf_path = Path(pf)
        if pf_path.is_dir():
            homes.extend(sorted(str(p) for p in pf_path.glob("Eclipse Adoptium/jdk-*")))
            homes.extend(sorted(str(p) for p in pf_path.glob("Java/jdk-*")))
            homes.extend(sorted(str(p) for p in pf_path.glob("Microsoft/jdk-*")))
    for home in homes:
        if home and java_major(Path(home) / "bin" / ("javac" + EXE)) >= 17:
            return Path(home) / "bin"
    javac = shutil.which("javac")
    if javac and java_major(javac) >= 17:
        return Path(javac).resolve().parent
    return None


def version_key(path):
    return [int(part) for part in re.findall(r"\d+", path.name)]


def find_toolchain(sdk):
    """The NDK's compiler directory for this host, or None without an NDK."""
    ndks = []
    if os.environ.get("ANDROID_NDK_HOME"):
        ndks.append(Path(os.environ["ANDROID_NDK_HOME"]))
    ndks += sorted((sdk / "ndk").glob("*"), key=version_key, reverse=True)
    for ndk in ndks:
        for clang in sorted((ndk / "toolchains" / "llvm" / "prebuilt").glob("*/bin/clang" + EXE)):
            return clang.parent
    return None


def short_path(path):
    """The 8.3 form of a Windows path; the NDK's .cmd wrappers break on spaces."""
    if sys.platform != "win32":
        return str(path)
    import ctypes
    buffer = ctypes.create_unicode_buffer(32768)
    length = ctypes.windll.kernel32.GetShortPathNameW(str(path), buffer, len(buffer))
    return buffer.value if 0 < length < len(buffer) else str(path)


def ndk_compiler(toolchain, prefix):
    # The per-target wrapper, not clang --target: Go probes linker flags without the
    # extra argument, which drops the build-id and exports cgo internals on armeabi-v7a.
    wrapper = Path(short_path(toolchain)) / f"{prefix}{MIN_SDK}-clang{'.cmd' if sys.platform == 'win32' else ''}"
    return f'"{wrapper}"'


def missing():
    """What the APK build lacks, as readable items; empty when it can run."""
    problems = []
    if find_sdk() is None:
        problems.append(f"Android SDK with platforms;{PLATFORM} and build-tools;{BUILD_TOOLS} (set ANDROID_HOME)")
    if find_jdk() is None:
        problems.append("JDK 17 or newer (set JAVA_HOME)")
    if shutil.which("go") is None:
        problems.append("Go")
    return problems


def app_version():
    source = (APP / "internal" / "store" / "store.go").read_text(encoding="utf-8")
    name = re.search(r'^const Version = "([^"]+)"', source, re.M).group(1)
    major, minor, patch = (int(part) for part in (name.split(".") + ["0", "0"])[:3])
    return name, major * 10000 + minor * 100 + patch


def run(*command, **kwargs):
    subprocess.run([str(part) for part in command], check=True, **kwargs)


def go_env(**extra):
    # A desktop cross-build's settings must not reach the per-ABI builds.
    env = {k: v for k, v in os.environ.items() if k not in ("GOOS", "GOARCH", "GOARM", "CGO_ENABLED", "CC", "CXX")}
    env.update(GOOS="android", **extra)
    return env


def build_libraries(sdk, log):
    toolchain = find_toolchain(sdk)
    if toolchain is None:
        log("No NDK: building only a cgo-less arm64 library (Go's own DNS, no armeabi-v7a or x86_64)")
        targets = [(abi, go_env(GOARCH="arm64", CGO_ENABLED="0")) for abi, *_ in ABIS[:1]]
    else:
        # Go splits CC like a shell, so the quotes keep a path with spaces intact.
        targets = [(abi, go_env(GOARCH=arch, CGO_ENABLED="1", CC=ndk_compiler(toolchain, prefix), **extra))
                   for abi, arch, extra, prefix in ABIS]
    for abi, env in targets:
        log(f"Building the {abi} library")
        library = OUT / "apk" / "lib" / abi / "libclashvless.so"
        library.parent.mkdir(parents=True, exist_ok=True)
        run("go", "-C", APP, "build", "-trimpath", "-ldflags=-s -w", "-o", library, ".", env=env)


def stage_branding():
    branding = APP / "branding"
    for folder, name, source in (("drawable-nodpi", "clashvless_logo.png", "logo.png"),
                                 ("mipmap-nodpi", "clashvless_launcher.png", "logo.png"),
                                 ("drawable-nodpi", "clashvless_notification.png", "notification.png")):
        target = OUT / "res" / folder / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(branding / "png" / source, target)
    assets = OUT / "apk" / "assets" / "branding"
    (assets / "png").mkdir(parents=True, exist_ok=True)
    shutil.copyfile(branding / "tokens.json", assets / "tokens.json")
    for png in (branding / "png").glob("*.png"):
        shutil.copyfile(png, assets / "png" / png.name)


def build(log=print):
    """Build and sign the APK; returns its path. Raises on a missing tool or a failed step."""
    problems = missing()
    if problems:
        raise RuntimeError("cannot build the APK; missing: " + "; ".join(problems))
    sdk, jdk = find_sdk(), find_jdk()
    tools = sdk / "build-tools" / BUILD_TOOLS
    android_jar = sdk / "platforms" / PLATFORM / "android.jar"

    def java(name):
        # Windows' Oracle "javapath" shim holds java and javac but not keytool.
        tool = jdk / (name + EXE)
        return tool if tool.is_file() else shutil.which(name) or tool

    name, code = app_version()
    log(f"Android SDK {sdk}, JDK {jdk}, version {name} ({code})")

    shutil.rmtree(OUT, ignore_errors=True)
    (OUT / "classes").mkdir(parents=True)
    DIST.mkdir(parents=True, exist_ok=True)
    build_libraries(sdk, log)
    stage_branding()

    log("Linking resources")
    aapt2 = tools / ("aapt2" + EXE)
    run(aapt2, "compile", "--dir", OUT / "res", "-o", OUT / "res.zip")
    run(aapt2, "compile", "--dir", ANDROID / "res", "-o", OUT / "app-res.zip")
    run(aapt2, "link", "-I", android_jar, "--manifest", ANDROID / "AndroidManifest.xml", "--java", OUT / "generated",
        "--min-sdk-version", MIN_SDK, "--target-sdk-version", TARGET_SDK,
        "--version-name", name, "--version-code", code,
        "-o", OUT / "base.apk", OUT / "res.zip", OUT / "app-res.zip")

    log("Compiling Java")
    sources = sorted([*(ANDROID / "src").rglob("*.java"), *(OUT / "generated").rglob("*.java")])
    argfile = OUT / "sources.txt"  # keeps the command under Windows' length limit
    argfile.write_text("\n".join('"' + path.as_posix() + '"' for path in sources), encoding="utf-8")
    run(java("javac"), "-encoding", "UTF-8", "-nowarn", "--release", "17", "-classpath", android_jar,
        "-d", OUT / "classes", "@" + str(argfile))
    log("Running scan & import tests")
    run(java("java"), "-cp", f"{OUT / 'classes'}{os.pathsep}{android_jar}", "com.clashvless.ScanImportTest")
    classes = OUT / "classes.jar"
    with zipfile.ZipFile(classes, "w") as jar:
        for path in sorted((OUT / "classes").rglob("*.class")):
            jar.write(path, path.relative_to(OUT / "classes").as_posix())
    run(java("java"), "-cp", tools / "lib" / "d8.jar", "com.android.tools.r8.D8", "--release",
        "--min-api", MIN_SDK, "--lib", android_jar, "--output", OUT / "apk", classes)

    log("Packaging")
    with zipfile.ZipFile(OUT / "base.apk", "a", zipfile.ZIP_DEFLATED) as apk:
        for path in sorted((OUT / "apk").rglob("*")):
            if path.is_file():
                apk.write(path, path.relative_to(OUT / "apk").as_posix())
    run(tools / ("zipalign" + EXE), "-f", "-p", "4", OUT / "base.apk", OUT / "aligned.apk")

    keystore = Path(os.environ.get("CLASHVLESS_KEYSTORE") or Path.home() / ".android" / "clashvless.keystore")
    if not keystore.is_file():
        log(f"Creating the signing key {keystore}")
        keystore.parent.mkdir(parents=True, exist_ok=True)
        run(java("keytool"), "-genkeypair", "-keystore", keystore, "-storepass", "android", "-keypass", "android",
            "-alias", "clashvless", "-keyalg", "RSA", "-keysize", "2048", "-validity", "10000",
            "-dname", "CN=clashvless", stdout=subprocess.DEVNULL)
    output = DIST / "clashvless.apk"
    run(java("java"), "-cp", tools / "lib" / "apksigner.jar", "com.android.apksigner.ApkSignerTool", "sign",
        "--ks", keystore, "--ks-pass", "pass:android", "--out", output, OUT / "aligned.apk")
    log(f"Built {output} ({name})")
    return output


if __name__ == "__main__":
    try:
        build()
    except (RuntimeError, OSError, subprocess.CalledProcessError) as exc:
        print(f"APK build failed: {exc}", file=sys.stderr)
        sys.exit(1)
