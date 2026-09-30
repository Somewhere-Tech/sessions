package tech.somewhere.sessions

import android.os.Bundle
import android.os.SystemClock
import android.webkit.WebView
import android.widget.Toast
import androidx.activity.OnBackPressedCallback
import androidx.activity.enableEdgeToEdge
import androidx.core.view.ViewCompat
import androidx.core.view.WindowInsetsCompat
import androidx.core.view.WindowInsetsControllerCompat

class MainActivity : TauriActivity() {
  private lateinit var webView: WebView
  private var backDecisionPending = false
  private var firstRootBackAt = 0L
  private var exitToast: Toast? = null

  override fun onCreate(savedInstanceState: Bundle?) {
    enableEdgeToEdge()
    super.onCreate(savedInstanceState)
  }

  override fun onWebViewCreate(webView: WebView) {
    this.webView = webView
    onBackPressedDispatcher.addCallback(this, object : OnBackPressedCallback(true) {
      override fun handleOnBackPressed() {
        if (dismissKeyboard()) {
          resetExitConfirmation()
          return
        }
        if (backDecisionPending) return

        backDecisionPending = true
        webView.evaluateJavascript(
          """
          (() => {
            try {
              return window.__SESSIONS_ANDROID_BACK__?.() === true;
            } catch (_) {
              return false;
            }
          })()
          """.trimIndent()
        ) { handled ->
          backDecisionPending = false
          if (handled == "true") {
            resetExitConfirmation()
          } else {
            handleRootBack()
          }
        }
      }
    })
  }

  private fun dismissKeyboard(): Boolean {
    if (!::webView.isInitialized) return false
    val insets = ViewCompat.getRootWindowInsets(webView) ?: return false
    if (!insets.isVisible(WindowInsetsCompat.Type.ime())) return false
    WindowInsetsControllerCompat(window, webView).hide(WindowInsetsCompat.Type.ime())
    return true
  }

  private fun handleRootBack() {
    val now = SystemClock.elapsedRealtime()
    if (firstRootBackAt != 0L && now - firstRootBackAt <= EXIT_CONFIRMATION_MS) {
      exitToast?.cancel()
      finish()
      return
    }

    firstRootBackAt = now
    exitToast?.cancel()
    exitToast = Toast.makeText(this, "Press back again to exit", Toast.LENGTH_SHORT).also {
      it.show()
    }
  }

  private fun resetExitConfirmation() {
    firstRootBackAt = 0L
    exitToast?.cancel()
    exitToast = null
  }

  companion object {
    private const val EXIT_CONFIRMATION_MS = 2_000L
  }
}
