#import <UIKit/UIKit.h>
#import <WebKit/WebKit.h>
#import <QuartzCore/QuartzCore.h>

// A dependency-free shell: one WKWebView pointed at the deployed web UI.
// Deliberately Objective-C, no Swift runtime, no frameworks, and built with an
// older toolchain so third-party signers (轻松签/Sideloadly) can re-sign it.
@interface AppDelegate : UIResponder <UIApplicationDelegate>
@property(strong, nonatomic) UIWindow *window;
@end

@implementation AppDelegate

- (BOOL)application:(UIApplication *)application
    didFinishLaunchingWithOptions:(NSDictionary *)launchOptions {
  self.window = [[UIWindow alloc] initWithFrame:[UIScreen mainScreen].bounds];

  WKWebViewConfiguration *configuration = [[WKWebViewConfiguration alloc] init];
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
  web.opaque = NO;
  web.backgroundColor = [UIColor clearColor];
  // ProMotion: ask for the full 120Hz range. Without this (and the
  // CADisableMinimumFrameDurationOnPhone plist key) WKWebView is capped at 60.
  if (@available(iOS 15.0, *)) {
    web.scrollView.preferredFrameRateRange =
        CAFrameRateRangeMake(80.0, 120.0, 120.0);
  }

  UIViewController *controller = [[UIViewController alloc] init];
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
