package tech.somewhere.sessions

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.AtomicFile
import java.io.ByteArrayOutputStream
import java.io.File
import java.io.FileOutputStream
import java.security.KeyStore
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey

internal class AndroidMachineVault(context: Context) : MachineVaultStorage {
  private val file = AtomicFile(File(context.noBackupFilesDir, "machine-credentials-v1.enc"))
  private val alias = "${context.packageName}.machine-credentials.v1"
  private val cipher = MachineVaultCipher(alias.toByteArray(Charsets.UTF_8))

  override fun read(): String? {
    // AtomicFile can restore a previous .bak; don't misclassify it as absent.
    if (!file.baseFile.exists() && !File("${file.baseFile.path}.bak").exists()) return null
    val encoded = file.openRead().use { input ->
      val output = ByteArrayOutputStream()
      val buffer = ByteArray(8192)
      while (true) {
        val count = input.read(buffer)
        if (count < 0) break
        if (output.size() + count > MachineVaultCipher.MAX_CIPHERTEXT) {
          throw MachineVaultException("The protected credential store is too large. Contact support before changing saved machines.")
        }
        output.write(buffer, 0, count)
      }
      output.toByteArray()
    }
    return cipher.decrypt(encoded, key(create = false))
  }

  override fun write(vault: String) {
    val encoded = cipher.encrypt(vault, key(create = true))
    var stream: FileOutputStream? = null
    try {
      stream = file.startWrite()
      stream.write(encoded)
      stream.fd.sync()
      file.finishWrite(stream)
    } catch (error: Exception) {
      file.failWrite(stream)
      throw error
    }
  }

  override fun remove() {
    // Only rollback of this transaction's new file, never a paired host revoke.
    file.delete()
    if (file.baseFile.exists() || File("${file.baseFile.path}.bak").exists()) {
      throw MachineVaultException("The unverified credential update could not be removed.")
    }
  }

  private fun key(create: Boolean): SecretKey {
    val store = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
    val existing = store.getKey(alias, null)
    if (existing is SecretKey) return existing
    if (existing != null || !create || file.baseFile.exists() || File("${file.baseFile.path}.bak").exists()) {
      // Never regenerate a missing key over existing ciphertext.
      throw MachineVaultException("The protected credential key is unavailable. Unlock this device and reopen Sessions; do not clear app data.")
    }
    val generator = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore")
    generator.init(KeyGenParameterSpec.Builder(alias, KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT)
      .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
      .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
      .setKeySize(256)
      .setRandomizedEncryptionRequired(true)
      .build())
    return generator.generateKey()
  }
}
