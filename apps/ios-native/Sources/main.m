#import <UIKit/UIKit.h>
#import <WebKit/WebKit.h>
#import <QuartzCore/QuartzCore.h>

// A dependency-free shell: one WKWebView pointed at the deployed web UI.
// Deliberately Objective-C, no Swift runtime, no frameworks, and built with an
// older toolchain so third-party signers (轻松签/Sideloadly) can re-sign it.
// JS -> native haptics: window.webkit.messageHandlers.haptic.postMessage('light')
@interface HapticHandler : NSObject <WKScriptMessageHandler>
@end

@implementation HapticHandler

- (void)userContentController:(WKUserContentController *)userContentController
      didReceiveScriptMessage:(WKScriptMessage *)message {
  NSString *kind = [message.body isKindOfClass:[NSString class]]
                       ? (NSString *)message.body
                       : @"light";
  dispatch_async(dispatch_get_main_queue(), ^{
    if ([kind isEqualToString:@"success"] ||
        [kind isEqualToString:@"warning"] ||
        [kind isEqualToString:@"error"]) {
      UINotificationFeedbackGenerator *generator =
          [[UINotificationFeedbackGenerator alloc] init];
      UINotificationFeedbackType type = UINotificationFeedbackTypeSuccess;
      if ([kind isEqualToString:@"warning"]) {
        type = UINotificationFeedbackTypeWarning;
      } else if ([kind isEqualToString:@"error"]) {
        type = UINotificationFeedbackTypeError;
      }
      [generator notificationOccurred:type];
      return;
    }
    UIImpactFeedbackStyle style = [kind isEqualToString:@"medium"]
                                      ? UIImpactFeedbackStyleMedium
                                      : UIImpactFeedbackStyleLight;
    UIImpactFeedbackGenerator *generator =
        [[UIImpactFeedbackGenerator alloc] initWithStyle:style];
    [generator impactOccurred];
  });
}

@end

// Keeps the status bar legible against the dynamic page background.
@interface RootViewController : UIViewController
@end

@implementation RootViewController

- (UIStatusBarStyle)preferredStatusBarStyle {
  if (@available(iOS 13.0, *)) {
    return self.traitCollection.userInterfaceStyle == UIUserInterfaceStyleDark
               ? UIStatusBarStyleLightContent
               : UIStatusBarStyleDarkContent;
  }
  return UIStatusBarStyleDefault;
}

- (void)traitCollectionDidChange:(UITraitCollection *)previousTraitCollection {
  [super traitCollectionDidChange:previousTraitCollection];
  [self setNeedsStatusBarAppearanceUpdate];
}

@end

@interface AppDelegate : UIResponder <UIApplicationDelegate>
@property(strong, nonatomic) UIWindow *window;
@end

@implementation AppDelegate

- (BOOL)application:(UIApplication *)application
    didFinishLaunchingWithOptions:(NSDictionary *)launchOptions {
  self.window = [[UIWindow alloc] initWithFrame:[UIScreen mainScreen].bounds];

  WKWebViewConfiguration *configuration = [[WKWebViewConfiguration alloc] init];
  static HapticHandler *haptics;
  haptics = [[HapticHandler alloc] init];
  [configuration.userContentController addScriptMessageHandler:haptics
                                                         name:@"haptic"];
  configuration.allowsInlineMediaPlayback = YES;
  configuration.mediaTypesRequiringUserActionForPlayback =
      WKAudiovisualMediaTypeNone;

  WKWebView *web = [[WKWebView alloc] initWithFrame:self.window.bounds
                                     configuration:configuration];
  web.autoresizingMask =
      UIViewAutoresizingFlexibleWidth | UIViewAutoresizingFlexibleHeight;
  web.allowsBackForwardNavigationGestures = YES;
  // The page owns safe-area insets and the 100dvh layout.
  web.scrollView.contentInsetAdjustmentBehavior =
      UIScrollViewContentInsetAdjustmentNever;
  UIColor *background = [UIColor colorWithDynamicProvider:^UIColor *(
      UITraitCollection *trait) {
    if (trait.userInterfaceStyle == UIUserInterfaceStyleDark) {
      return [UIColor colorWithRed:26.0 / 255.0
                             green:25.0 / 255.0
                              blue:23.0 / 255.0
                             alpha:1.0];
    }
    return [UIColor colorWithRed:239.0 / 255.0
                           green:238.0 / 255.0
                            blue:233.0 / 255.0
                           alpha:1.0];
  }];
  // The keyboard shrinks the web view; whatever is behind it must be the page
  // background, not the default black window.
  web.opaque = YES;
  web.backgroundColor = background;
  web.scrollView.backgroundColor = background;
  web.scrollView.keyboardDismissMode = UIScrollViewKeyboardDismissModeInteractive;
  web.scrollView.automaticallyAdjustsScrollIndicatorInsets = NO;
  self.window.backgroundColor = background;
  RootViewController *controller = [[RootViewController alloc] init];
  controller.view.backgroundColor = background;
  controller.view = web;
  self.window.rootViewController = controller;
  [self.window makeKeyAndVisible];

  NSURL *url = [NSURL URLWithString:@"http://8.166.138.207/code/"];
  // Always revalidate the HTML; hashed assets are immutable. A cached
  // index.html is what makes a freshly deployed UI look unchanged.
  [web loadRequest:[NSURLRequest requestWithURL:url
                                     cachePolicy:NSURLRequestReloadIgnoringLocalCacheData
                                 timeoutInterval:30]];
  return YES;
}

@end

int main(int argc, char *argv[]) {
  @autoreleasepool {
    return UIApplicationMain(argc, argv, nil,
                             NSStringFromClass([AppDelegate class]));
  }
}
