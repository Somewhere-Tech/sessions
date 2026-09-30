package tech.somewhere.sessions

import java.nio.ByteBuffer
import java.nio.charset.CodingErrorAction
import javax.crypto.Cipher
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

internal class MachineVaultCipher(private val associatedData: ByteArray) {
  fun encrypt(vault: String, key: SecretKey): ByteArray {
    val plaintext = vault.toByteArray(Charsets.UTF_8)
    require(plaintext.size <= MAX_PLAINTEXT) { "Credential store is too large." }
    val cipher = Cipher.getInstance("AES/GCM/NoPadding")
    // Let the provider generate a fresh random nonce; never reuse a supplied IV.
    cipher.init(Cipher.ENCRYPT_MODE, key)
    cipher.updateAAD(associatedData)
    require(cipher.iv.size == IV_BYTES) { "Unexpected credential encryption nonce." }
    return MAGIC + cipher.iv + cipher.doFinal(plaintext)
  }

  fun decrypt(encoded: ByteArray, key: SecretKey): String {
    require(encoded.size in MIN_BYTES..MAX_CIPHERTEXT) { "Invalid protected credential size." }
    require(encoded.copyOfRange(0, MAGIC.size).contentEquals(MAGIC)) { "Unsupported protected credential format." }
    val cipher = Cipher.getInstance("AES/GCM/NoPadding")
    val iv = encoded.copyOfRange(MAGIC.size, MAGIC.size + IV_BYTES)
    cipher.init(Cipher.DECRYPT_MODE, key, GCMParameterSpec(128, iv))
    cipher.updateAAD(associatedData)
    val plaintext = cipher.doFinal(encoded.copyOfRange(MAGIC.size + IV_BYTES, encoded.size))
    return Charsets.UTF_8.newDecoder().onMalformedInput(CodingErrorAction.REPORT)
      .onUnmappableCharacter(CodingErrorAction.REPORT).decode(ByteBuffer.wrap(plaintext)).toString()
  }

  companion object {
    const val MAX_PLAINTEXT = 128 * 1024
    private const val IV_BYTES = 12
    private val MAGIC = byteArrayOf(0x53, 0x4d, 0x56, 0x31) // SMV1
    private val MIN_BYTES = MAGIC.size + IV_BYTES + 16
    val MAX_CIPHERTEXT = MAX_PLAINTEXT + MAGIC.size + IV_BYTES + 16
  }
}
