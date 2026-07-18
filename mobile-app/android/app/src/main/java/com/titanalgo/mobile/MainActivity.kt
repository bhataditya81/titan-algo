package com.titanalgo.mobile

import android.annotation.SuppressLint
import android.os.Bundle
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.appcompat.app.AppCompatActivity
import mobile.Mobile // Gomobile binding

class MainActivity : AppCompatActivity() {

    private lateinit var webView: WebView

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        
        // 1. Start Go Engine (in background)
        Thread {
            try {
                // filesDir.absolutePath is the internal storage for app
                // "titan-mobile-secret" matches the API key in app.js
                val result = Mobile.start(filesDir.absolutePath, "titan-mobile-secret")
                println("TitanEngine Start Result: $result")
            } catch (e: Exception) {
                e.printStackTrace()
            }
        }.start()

        // 2. Setup WebView
        webView = WebView(this)
        setContentView(webView)

        // Configure WebView settings
        webView.settings.apply {
            javaScriptEnabled = true
            domStorageEnabled = true
            allowFileAccess = true
            allowContentAccess = true
            cacheMode = WebSettings.LOAD_DEFAULT
            mixedContentMode = WebSettings.MIXED_CONTENT_ALWAYS_ALLOW
            useWideViewPort = true
            loadWithOverviewMode = true
            setSupportZoom(false)
            builtInZoomControls = false
        }

        // Stay within WebView (don't open in browser)
        webView.webViewClient = WebViewClient()

        // Load local assets
        // The Go server runs on localhost:8080, but we load the UI from assets
        // The UI (app.js) calls http://localhost:8080/api/...
        webView.loadUrl("file:///android_asset/www/index.html")
    }

    override fun onDestroy() {
        super.onDestroy()
        try {
            Mobile.stop()
        } catch (e: Exception) {
            e.printStackTrace()
        }
    }

    override fun onBackPressed() {
        if (webView.canGoBack()) {
            webView.goBack()
        } else {
            super.onBackPressed()
        }
    }
}
