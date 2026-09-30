package tech.somewhere.sessions

import android.app.Activity
import android.app.Instrumentation
import android.os.Bundle
import java.io.File
import java.security.KeyStore

// SDK-only instrumentation of the exact production vault classes. This app has
// a distinct package/key alias and no network permission or real credentials.
class VaultInstrumentation : Instrumentation() {
  private var phase = ""
  private val fixture = """{"version":1,"credentials":[{"serverId":"synthetic-host","token":"fixture-not-a-real-token"}]}"""

  override fun onCreate(arguments: Bundle?) {
    phase = arguments?.getString("phase") ?: ""
    super.onCreate(arguments)
    start()
  }

  override fun onStart() {
    val alias = "${targetContext.packageName}.machine-credentials.v1"
    val file = File(targetContext.noBackupFilesDir, "machine-credentials-v1.enc")
    try {
      check(targetContext.packageName == "tech.somewhere.sessions.vault.fixture")
      val backend = AndroidMachineVault(targetContext)
      val store = MachineVaultTransaction(backend) { check(it.contains("\"version\":1")) }
      when (phase) {
        "seed" -> {
          check(store.load() == null)
          check(store.save(fixture) == fixture)
          check(!String(file.readBytes(), Charsets.ISO_8859_1).contains("fixture-not-a-real-token"))
          val key = keys().getKey(alias, null)
          check(key != null && key.encoded == null) // AndroidKeyStore is non-exportable.
        }
        "reopen" -> {
          check(store.load() == fixture) // Fresh instrumentation process.
          val initial = file.readBytes()
          file.writeBytes(initial.clone().apply { this[lastIndex] = (this[lastIndex].toInt() xor 1).toByte() })
          val damaged = file.readBytes()
          fails { store.load() }
          fails { store.save(fixture) }
          check(file.readBytes().contentEquals(damaged)) // Unreadable != empty.
          file.writeBytes(initial)
          check(store.load() == fixture)
          verifyRollback(backend, file)
          val confirmed = file.readBytes()
          keys().deleteEntry(alias)
          fails { store.load() }
          fails { store.save(fixture) }
          check(!keys().containsAlias(alias)) // No key regeneration over ciphertext.
          check(file.readBytes().contentEquals(confirmed))
          backend.remove()
          check(store.load() == null)
        }
        else -> error("Unknown fixture phase")
      }
      finish(Activity.RESULT_OK, Bundle().apply { putString("stream", "Android Keystore fixture $phase passed\n") })
    } catch (_: Exception) {
      finish(Activity.RESULT_CANCELED, Bundle().apply { putString("stream", "Android Keystore fixture $phase failed\n") })
    } finally {
      if (phase != "seed") {
        // Exact disposable fixture files and alias only, never Sessions app data.
        file.delete()
        File("${file.path}.bak").delete()
        File("${file.path}.new").delete()
        keys().deleteEntry(alias)
      }
    }
  }

  private fun verifyRollback(backend: AndroidMachineVault, file: File) {
    var corruptOnce = true
    val faulty = object : MachineVaultStorage {
      override fun read() = backend.read()
      override fun remove() = backend.remove()
      override fun write(vault: String) {
        backend.write(vault)
        if (corruptOnce) {
          corruptOnce = false
          val bytes = file.readBytes()
          bytes[bytes.lastIndex] = (bytes.last().toInt() xor 1).toByte()
          file.writeBytes(bytes)
        }
      }
    }
    val transaction = MachineVaultTransaction(faulty) { check(it.contains("\"version\":1")) }
    fails { transaction.save("""{"version":1,"credentials":[]}""") }
    check(transaction.load() == fixture)
  }

  private fun keys() = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
  private fun fails(action: () -> Unit) {
    var rejected = false
    try { action() } catch (_: Exception) { rejected = true }
    check(rejected)
  }
}
