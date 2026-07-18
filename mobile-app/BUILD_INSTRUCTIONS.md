# Android Build & Install Guide

## Prerequisites

1.  **Go 1.21+** installed (`go version` to verify).
2.  **Android Studio** installed.
3.  **Gomobile** toolchain installed.

## Step 1: Install Gomobile (One-time setup)

Open your terminal (PowerShell or CMD) and run:

```powershell
go install golang.org/x/mobile/cmd/gomobile@latest
gomobile init
```

*Note: If `gomobile` command is not found, ensure `%USERPROFILE%\go\bin` is in your system PATH.*

## Step 2: Generate the Trading Engine Library

We need to compile the Go code into an Android library (`.aar`).

1.  Open terminal in `titan-algo/go-engine`.
2.  Run the bind command:

```powershell
gomobile bind -o ../mobile-app/android/app/libs/mobile.aar -target=android ./mobile
```

*This process might take a minute. It creates `mobile.aar` in the android libs folder.*

## Step 3: Open in Android Studio

1.  Launch **Android Studio**.
2.  Select **Open** (or File > Open).
3.  Navigate to: `titan-algo/mobile-app/android`
4.  Click **OK**.
5.  Wait for Gradle Sync to complete (look at the bottom status bar).

## Step 4: Build and Install

1.  Connect your Android phone via USB (ensure **USB Debugging** is on in Developer Options).
    *   *Alternatively, set up an Android Emulator in Device Manager.*
2.  In the toolbar, ensure your device is selected.
3.  Click the green **Run** (Play) button ▶️.

Android Studio will compile the app, install it on your device, and launch it.

## Step 5: Start Trading

1.  The app will open.
2.  It automatically starts the embedded Go engine.
3.  You will see the "Connected" status.
4.  Go to **Config** tab to set up your strategy or check **Dashboard** to see status.

## Troubleshooting

### "no usable NDK" Error?
This means the Android Native Development Kit is missing.

**Fix:**
1. Open **Android Studio**.
2. Go to **Tools > SDK Manager**.
3. Click the **SDK Tools** tab.
4. Check the box for **NDK (Side by side)** and **CMake**.
5. Click **Apply** to install.
6. Once installed, note the path (usually `C:\Users\<user>\AppData\Local\Android\Sdk\ndk\<version>`).
7. Run:
   ```powershell
   gomobile init -ndk "C:\Users\bhata\AppData\Local\Android\Sdk\ndk\<version>"
   ```

*   **"mobile.aar not found"**: Review Step 2. The file MUST exist at `mobile-app/android/app/libs/mobile.aar`.
