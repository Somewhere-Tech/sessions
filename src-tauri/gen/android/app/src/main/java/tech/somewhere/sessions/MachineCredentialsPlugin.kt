package tech.somewhere.sessions

import android.app.Activity
import androidx.appcompat.app.AppCompatActivity
import app.tauri.annotation.Command
import app.tauri.annotation.InvokeArg
import app.tauri.annotation.TauriPlugin
import app.tauri.plugin.Invoke
import app.tauri.plugin.JSObject
import app.tauri.plugin.Plugin
import org.json.JSONObject
import java.util.concurrent.Executors

@InvokeArg
class MachineCredentialSaveArgs { lateinit var vault: String }

@TauriPlugin
class MachineCredentialsPlugin(activity: Activity) : Plugin(activity) {
  // Keystore and synced file operations never run on Android's UI thread.
  private val executor = Executors.newSingleThreadExecutor()
  private val vault = MachineVaultTransaction(AndroidMachineVault(activity), ::validateVault)

  override fun onDestroy(activity: AppCompatActivity) {
    executor.shutdown() // Finish admitted writes, but do not retain an old Activity.
    super.onDestroy(activity)
  }

  @Command
  fun loadMachineCredentials(invoke: Invoke) = respond(invoke) { vault.load() }

  @Command
  fun saveMachineCredentials(invoke: Invoke) {
    val args = try { invoke.parseArgs(MachineCredentialSaveArgs::class.java) } catch (_: Exception) {
      invoke.reject("Sessions could not prepare the protected credential update.")
      return
    }
    respond(invoke) { vault.save(args.vault) }
  }

  private fun respond(invoke: Invoke, action: () -> String?) {
    executor.execute {
      try {
        invoke.resolve(JSObject().put("vault", action() ?: JSONObject.NULL))
      } catch (error: Exception) {
        // Never serialize exception text from crypto, JSON, or file APIs: it
        // can contain plaintext inputs or filenames. Own errors contain no data.
        invoke.reject(if (error is MachineVaultException) error.message else
          "Sessions could not access the Android protected credentials. Unlock this device and reopen Sessions. Saved connections have not been removed.")
      }
    }
  }

  private fun validateVault(encoded: String) {
    if (encoded.toByteArray(Charsets.UTF_8).size > MachineVaultCipher.MAX_PLAINTEXT) {
      throw MachineVaultException("The protected machine credential store is too large.")
    }
    val data = JSONObject(encoded)
    if (data.get("version") != 1) throw MachineVaultException("This protected credential store needs a newer Sessions app.")
    val credentials = data.getJSONArray("credentials")
    if (credentials.length() > 100) throw MachineVaultException("Sessions can protect at most 100 saved machines.")
    val seen = HashSet<String>()
    for (index in 0 until credentials.length()) {
      val credential = credentials.getJSONObject(index)
      val server = credential.get("serverId") as? String
        ?: throw MachineVaultException("The protected machine credential store contains an invalid machine.")
      val token = credential.get("token") as? String
        ?: throw MachineVaultException("The protected machine credential store contains an invalid credential.")
      if (!valid(server, 128) || !valid(token, 512) || !seen.add(server)) {
        throw MachineVaultException("The protected machine credential store contains an invalid machine or credential.")
      }
    }
  }

  private fun valid(value: String, maximum: Int): Boolean = value.isNotEmpty()
    && value.toByteArray(Charsets.UTF_8).size <= maximum
    && value.none { Character.isISOControl(it) }
}
