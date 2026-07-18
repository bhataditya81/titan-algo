# TitanAlgo Mobile App

A minimal Android app to remotely control the TitanAlgo trading engine from your mobile device.

## Architecture

**Self-Sufficient (Gomobile)**
- Go engine compiled to Native Library (`.aar`)
- Embedded inside Android APK
- Runs local HTTP server on phone (`localhost:8080`)
- No PC dependency - run trading entirely on phone!

## Prerequisites

1. **Android Studio**
2. **Go 1.21+**
3. **Gomobile** tool chain:
   ```bash
   go install golang.org/x/mobile/cmd/gomobile@latest
   gomobile init
   ```

## Build Instructions

### Step 1: Generate Go Library

Compile the Go engine into an Android Archive (`.aar`):

```bash
cd go-engine
gomobile bind -o ../mobile-app/android/app/libs/mobile.aar -target=android ./mobile
```

This creates `mobile.aar` which contains the full trading engine!

### Step 2: Build APK

1. Open `mobile-app/android` in Android Studio
2. Sync Project with Gradle Files
3. **Build > Build APK**

### Step 3: Install & Run

1. Install APK on phone
2. Open App -> TitanAlgo starts automatically
3. Engine runs in background
4. Connects to Angel One directly from phone

## Usage

The app works exactly like the desktop version but runs entirely on your device.
- **Config**: Saves to app's internal storage
- **Logs**: Written to internal storage
- **Network**: Uses phone's 4G/5G/WiFi

## Troubleshooting

**Build Fails?**
- Ensure `gomobile init` ran successfully
- Check `android/app/libs/mobile.aar` exists
- Verify `NDK` is installed in Android Studio SDK Manager

**App Crashes on Start?**
- Check Logcat in Android Studio: `adb logcat | grep TianAlgo`
- Ensure "Internet" permission is granted


## License

Part of the TitanAlgo trading engine project.
