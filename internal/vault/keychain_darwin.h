// Native file-based Keychain operations shared with the isolated C fixture.
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>

// File-based Keychain supports unsigned checkout-local CLI binaries. Limit every
// operation to the default keychain plus the exact service/account primary key.
static OSStatus dm_query(SecKeychainRef selected, const char *account, Boolean interactive,
                        CFMutableDictionaryRef *out, SecKeychainRef *keychain) {
 OSStatus status = errSecSuccess;
 if (selected) { *keychain = (SecKeychainRef)CFRetain(selected); } else { status = SecKeychainCopyDefault(keychain); }
 if (status != errSecSuccess) return status;
 SecKeychainStatus state = 0;
 status = SecKeychainGetStatus(*keychain, &state);
 if (status != errSecSuccess) { CFRelease(*keychain); *keychain=NULL; return status; }
 if (!interactive && !(state & kSecUnlockStateStatus)) { CFRelease(*keychain); *keychain=NULL; return errSecInteractionNotAllowed; }
 *out = CFDictionaryCreateMutable(NULL, 0, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
 CFStringRef a = CFStringCreateWithCString(NULL, account, kCFStringEncodingUTF8);
 CFDictionarySetValue(*out, kSecClass, kSecClassGenericPassword);
 CFDictionarySetValue(*out, kSecAttrService, CFSTR("com.data-mate.vault"));
 CFDictionarySetValue(*out, kSecAttrAccount, a);
 CFRelease(a);

 return errSecSuccess;
}
static void dm_search(CFMutableDictionaryRef q, SecKeychainRef keychain) {
 const void *values[] = { keychain };
 CFArrayRef list = CFArrayCreate(NULL, values, 1, &kCFTypeArrayCallBacks);
 CFDictionarySetValue(q, kSecMatchSearchList, list);
 CFRelease(list);
}
static OSStatus dm_load(SecKeychainRef selected, const char *account, Boolean interactive, unsigned char *out) {
 CFMutableDictionaryRef q = NULL; SecKeychainRef keychain = NULL;
 OSStatus status = dm_query(selected, account, interactive, &q, &keychain);
 if (status != errSecSuccess) return status;
 dm_search(q, keychain);
 CFDictionarySetValue(q, kSecReturnData, kCFBooleanTrue);
 CFDictionarySetValue(q, kSecMatchLimit, kSecMatchLimitOne);
 CFTypeRef result = NULL;
 status = SecItemCopyMatching(q, &result);
 if (status == errSecSuccess) {
  if (!result || CFGetTypeID(result) != CFDataGetTypeID() || CFDataGetLength((CFDataRef)result) != 32) status = errSecDecode;
  else memcpy(out, CFDataGetBytePtr((CFDataRef)result), 32);
 }
 if (result) CFRelease(result);
 CFRelease(q); CFRelease(keychain);
 return status;
}
static OSStatus dm_create(SecKeychainRef selected, const char *account, Boolean interactive, const unsigned char *key) {
 CFMutableDictionaryRef q = NULL; SecKeychainRef keychain = NULL;
 OSStatus status = dm_query(selected, account, interactive, &q, &keychain);
 if (status != errSecSuccess) return status;
 CFDictionarySetValue(q, kSecUseKeychain, keychain);
 CFDataRef data = CFDataCreate(NULL, key, 32);
 CFDictionarySetValue(q, kSecValueData, data);
 status = SecItemAdd(q, NULL);
 CFRelease(data); CFRelease(q); CFRelease(keychain);
 return status;
}
static OSStatus dm_delete(SecKeychainRef selected, const char *account, Boolean interactive) {
 CFMutableDictionaryRef q = NULL; SecKeychainRef keychain = NULL;
 OSStatus status = dm_query(selected, account, interactive, &q, &keychain);
 if (status != errSecSuccess) return status;
 dm_search(q, keychain);
 // File-based Keychain deletion through the exact native reference avoids
 // SecItemDelete's invalid-owner-edit failure after an ad-hoc binary rebuild.
 CFDictionarySetValue(q, kSecReturnRef, kCFBooleanTrue);
 CFDictionarySetValue(q, kSecMatchLimit, kSecMatchLimitOne);
 CFTypeRef item = NULL;
 status = SecItemCopyMatching(q, &item);
 if (status == errSecSuccess) {
  if (!item || CFGetTypeID(item) != SecKeychainItemGetTypeID()) status = errSecDecode;
  else status = SecKeychainItemDelete((SecKeychainItemRef)item);
 }
 if (item) CFRelease(item);
 CFRelease(q); CFRelease(keychain);
 return status;
}
