package tech.somewhere.sessions

import org.junit.Assert.*
import org.junit.Test
import javax.crypto.KeyGenerator

class MachineVaultTest {
  private class Memory : MachineVaultStorage {
    var value: String? = null
    var unreadable = false
    var corruptOnce = false
    var writeDenied = false
    override fun read(): String? {
      if (unreadable) throw IllegalStateException("locked")
      return value
    }
    override fun write(vault: String) {
      if (writeDenied) throw IllegalStateException("denied")
      value = if (corruptOnce) { corruptOnce = false; "invalid" } else vault
    }
    override fun remove() { value = null }
  }

  private fun transaction(storage: Memory) = MachineVaultTransaction(storage) {
    require(it.startsWith("valid:")) { "invalid fixture" }
  }

  @Test fun verifiedRoundtripAndRemoval() {
    val backend = Memory()
    val store = transaction(backend)
    assertNull(store.load())
    assertEquals("valid:fake-device-token", store.save("valid:fake-device-token"))
    assertEquals("valid:fake-device-token", store.load())
    assertEquals("valid:empty", store.save("valid:empty"))
  }

  @Test fun unreadableStoreIsNotOverwritten() {
    val backend = Memory().apply { value = "valid:previous"; unreadable = true }
    try { transaction(backend).save("valid:new"); fail("accepted unreadable store") } catch (_: IllegalStateException) {}
    assertEquals("valid:previous", backend.value)
  }

  @Test fun corruptReadbackRestoresPrevious() {
    val backend = Memory().apply { value = "valid:previous"; corruptOnce = true }
    try { transaction(backend).save("valid:new"); fail("accepted corrupt write") } catch (error: MachineVaultException) {
      assertTrue(error.message!!.contains("previous set was restored"))
    }
    assertEquals("valid:previous", backend.value)
  }

  @Test fun firstFailedMigrationRemovesOnlyUnverifiedNewStore() {
    val backend = Memory().apply { corruptOnce = true }
    try { transaction(backend).save("valid:new"); fail("accepted corrupt write") } catch (_: MachineVaultException) {}
    assertNull(backend.value)
  }

  @Test fun deniedWriteKeepsPreviousAndReportsFailedRollback() {
    val backend = Memory().apply { value = "valid:previous"; writeDenied = true }
    try { transaction(backend).save("valid:new"); fail("accepted denied write") } catch (error: MachineVaultException) {
      assertTrue(error.message!!.contains("could not verify or restore"))
    }
    assertEquals("valid:previous", backend.value)
  }

  @Test fun aesGcmUsesRandomNonceAuthenticatesDataAndContainsNoPlaintext() {
    val key = KeyGenerator.getInstance("AES").apply { init(256) }.generateKey()
    val cipher = MachineVaultCipher("fixture-app".toByteArray())
    val plaintext = "valid:fake-device-token"
    val first = cipher.encrypt(plaintext, key)
    val second = cipher.encrypt(plaintext, key)
    assertFalse(first.contentEquals(second))
    assertFalse(String(first, Charsets.ISO_8859_1).contains("fake-device-token"))
    assertEquals(plaintext, cipher.decrypt(first, key))
    val tampered = first.clone().apply { this[lastIndex] = (this[lastIndex].toInt() xor 1).toByte() }
    try { cipher.decrypt(tampered, key); fail("accepted corrupted ciphertext") } catch (_: Exception) {}
    try { MachineVaultCipher("other-app".toByteArray()).decrypt(first, key); fail("accepted another app's ciphertext") } catch (_: Exception) {}
  }

  @Test fun cipherRejectsOversizedAndTruncatedData() {
    val key = KeyGenerator.getInstance("AES").apply { init(256) }.generateKey()
    val cipher = MachineVaultCipher("fixture-app".toByteArray())
    try { cipher.encrypt("x".repeat(MachineVaultCipher.MAX_PLAINTEXT + 1), key); fail("accepted oversized plaintext") } catch (_: IllegalArgumentException) {}
    try { cipher.decrypt(ByteArray(3), key); fail("accepted truncated ciphertext") } catch (_: IllegalArgumentException) {}
    try { cipher.decrypt(ByteArray(MachineVaultCipher.MAX_CIPHERTEXT + 1), key); fail("accepted oversized ciphertext") } catch (_: IllegalArgumentException) {}
  }
}
